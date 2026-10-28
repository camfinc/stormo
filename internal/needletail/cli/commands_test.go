package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// slowModel is echoModel, but a turn whose input ends in SLOWTURN waits until its request is
// cancelled (a /stop), telling started first.
type slowModel struct{ started chan struct{} }

func (m slowModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if bytes.Contains(body, []byte("SLOWTURN")) && !bytes.Contains(body, []byte(`"role":"tool"`)) {
		select {
		case m.started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	echoModel{}.ServeHTTP(w, r)
}

// The real serve's slash commands against the fake Slack: every answer the caller's alone
// (ephemeral, nothing posted), /memory scoped to the caller's DM (another person's records never
// shown), /trust refused to a non-approver and run (audited) for the approver on a thread a guest
// marked, /agent-status, /new restarting the DM's conversation unmarked, /stop cancelling a turn
// without an error note.
func TestServeSlackCommands(t *testing.T) {
	fake := slacktest.New(t)
	sm := slowModel{started: make(chan struct{}, 1)}
	model := httptest.NewServer(sm)
	defer model.Close()
	h := home(t, model.URL+"/v1", model.URL+"/v1")
	cfgPath := filepath.Join(h, "needletail.yaml")
	cfg, _ := os.ReadFile(cfgPath)
	if err := os.WriteFile(cfgPath, append(cfg, []byte("channels: [slack]\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	const (
		alice, bob, guest = "U0ALLOWED1", "U0BOBBOB01", "U0GUEST001"
		aliceDM, bobDM    = "D0ALICE001", "D0BOBBOB01"
		thread            = "1700000900.000100"
	)
	mem := &memory.Store{Home: h, Limits: map[string]int{"memory": 2000, "user": 2000}}
	for _, r := range []struct {
		text string
		a    memory.Audience
		sc   string
	}{
		{"Alice drinks green tea in the mornings.", memory.Audience{Scope: memory.DMScope(alice), Conv: "slack-" + aliceDM + "-dm"}, ""},
		{"Bob drinks black tea, never coffee.", memory.Audience{Scope: memory.DMScope(bob), Conv: "slack-" + bobDM + "-dm"}, ""},
		{"The office tea is jasmine.", memory.Audience{}, memory.ScopeAgent},
	} {
		if _, err := mem.Save(memory.SaveOptions{Text: r.text, Kind: memory.KindFact, Src: "chat", Audience: r.a, Scope: r.sc, Force: true}); err != nil {
			t.Fatal(err)
		}
	}
	setenv(t, h)
	for k, v := range map[string]string{"SLACK_BOT_TOKEN": slacktest.BotToken, "SLACK_APP_TOKEN": slacktest.AppToken,
		"SLACK_ALLOWED_USERS": alice + "," + bob, "SLACK_ALLOW_ALL_USERS": "true", "SLACK_HOME_CHANNEL": "C0HOME0001", SlackAPIEnv: fake.API()} {
		t.Setenv(k, v)
	}
	fake.DMUsers[aliceDM], fake.DMUsers[bobDM] = alice, bob
	fake.Profiles[guest] = map[string]any{"is_restricted": true}
	t.Setenv("NEEDLETAIL_API_PORT", strconv.Itoa(freePort(t)))
	var stderr bytes.Buffer
	var mu sync.Mutex
	logs := &lockedWriter{w: &stderr, mu: &mu}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- Run(ctx, []string{"serve"}, io.Discard, logs) }()
	defer func() {
		if t.Failed() {
			mu.Lock()
			t.Logf("serve log:\n%s", stderr.String())
			mu.Unlock()
		}
	}()
	fake.WaitConnected(10 * time.Second)
	n := 0
	ask := func(cmd, text, user, channel string, extra map[string]string) string {
		t.Helper()
		n++
		fake.Slash("C"+strconv.Itoa(n), cmd, text, user, channel, extra)
		res := fake.WaitResponses(n, 15*time.Second)
		r := res[n-1]
		if r.ResponseType != "ephemeral" || r.Auth != "" {
			t.Errorf("%s: answer %+v: not ephemeral", cmd, r)
		}
		return r.Text
	}
	// commands may answer before serve wires them: wait until they run
	deadline := time.Now().Add(10 * time.Second)
	for {
		if out := ask("/help", "", alice, aliceDM, nil); !strings.Contains(out, "not running yet") {
			if !strings.Contains(out, "/agent-status") {
				t.Fatalf("help: %q", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("commands never ran")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// a DM conversation, then the memory each person may see
	fake.Send("e1", slacktest.Event{Channel: aliceDM, ChannelType: "im", User: alice, Text: "hello", TS: "1700000800.000100"})
	fake.WaitPosts(1, 15*time.Second)
	out := ask("/memory", "tea", alice, aliceDM, nil)
	if !strings.Contains(out, "green tea") || !strings.Contains(out, "jasmine") || strings.Contains(out, "black tea") {
		t.Errorf("alice's /memory: %q", out)
	}
	out = ask("/memory", "tea", bob, bobDM, nil)
	if !strings.Contains(out, "black tea") || !strings.Contains(out, "jasmine") || strings.Contains(out, "green tea") {
		t.Errorf("bob's /memory: %q", out)
	}

	// a guest marks a channel thread; bob (no approver) may not endorse it, alice (the first
	// allowed user: the approver) does, audited
	fake.Send("e2", slacktest.Event{Channel: "C0DEVCHAN1", ChannelType: "channel", User: guest, Text: "<@" + slacktest.BotUser + "> hi there", TS: thread})
	fake.WaitPosts(2, 15*time.Second)
	sessFile := filepath.Join(h, "sessions", "slack-C0DEVCHAN1-"+thread+".jsonl")
	if b, _ := os.ReadFile(sessFile); !bytes.Contains(firstLine(b), []byte(`"untrusted":true`)) {
		t.Fatalf("the guest's thread is not marked: %s", firstLine(b))
	}
	if out := ask("/agent-status", "", alice, "C0DEVCHAN1", map[string]string{"thread_ts": thread}); !strings.Contains(out, "marked") || !strings.Contains(out, guest) {
		t.Errorf("status of the marked thread: %q", out)
	}
	if out := ask("/trust", "", bob, "C0DEVCHAN1", map[string]string{"thread_ts": thread}); !strings.Contains(out, "Only an approver") {
		t.Errorf("bob's /trust: %q", out)
	}
	if out := ask("/trust", "", alice, "C0DEVCHAN1", map[string]string{"thread_ts": thread}); !strings.Contains(out, "Endorsed") {
		t.Errorf("alice's /trust: %q", out)
	}
	if b, _ := os.ReadFile(sessFile); bytes.Contains(firstLine(b), []byte(`"untrusted":true`)) || !bytes.Contains(firstLine(b), []byte("slack:"+alice+" (/trust)")) {
		t.Errorf("after /trust: %s", firstLine(b))
	}
	if b, _ := os.ReadFile(filepath.Join(h, "state", "needletail", "approvals-audit.jsonl")); !bytes.Contains(b, []byte(`"event":"trusted"`)) || !bytes.Contains(b, []byte("slack:"+alice)) {
		t.Errorf("audit: %s", b)
	}
	if out := ask("/agent-status", "", alice, aliceDM, nil); !strings.Contains(out, "Model: gpt-6-luna") || !strings.Contains(out, "trusted") {
		t.Errorf("status: %q", out)
	}

	// /new: the DM goes on in a fresh, unmarked session; the old one ends "new"
	if out := ask("/new", "", alice, aliceDM, nil); !strings.Contains(out, "fresh conversation") {
		t.Fatalf("/new: %q", out)
	}
	dmRoot := filepath.Join(h, "sessions", "slack-"+aliceDM+"-dm.jsonl")
	var head wire.Session
	b, _ := os.ReadFile(dmRoot)
	_ = json.Unmarshal(firstLine(b), &head)
	if head.EndReason != wire.EndNew {
		t.Fatalf("old DM session: %s", firstLine(b))
	}
	found := false
	entries, _ := os.ReadDir(filepath.Join(h, "sessions"))
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(h, "sessions", e.Name()))
		var s wire.Session
		if json.Unmarshal(firstLine(b), &s) == nil && s.Parent == "slack-"+aliceDM+"-dm" {
			found = bytes.Contains(b, []byte("fresh conversation (/new)")) && !s.Untrusted
		}
	}
	if !found {
		t.Fatal("no fresh session after /new")
	}

	// /stop: the turn running in the DM is cancelled; no error note is posted
	posts := len(fake.Posts())
	fake.Send("e3", slacktest.Event{Channel: aliceDM, ChannelType: "im", User: alice, Text: "think hard SLOWTURN", TS: "1700000800.000200"})
	select {
	case <-sm.started:
	case <-time.After(15 * time.Second):
		t.Fatal("the slow turn never started")
	}
	if out := ask("/stop", "", alice, aliceDM, nil); !strings.Contains(out, "Stopped") {
		t.Errorf("/stop: %q", out)
	}
	time.Sleep(500 * time.Millisecond)
	if p := fake.Posts(); len(p) != posts {
		t.Errorf("posts after /stop: %+v", p[posts:])
	}
	if out := ask("/stop", "", alice, aliceDM, nil); !strings.Contains(out, "No turn is running") {
		t.Errorf("second /stop: %q", out)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func firstLine(b []byte) []byte {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return b[:i]
	}
	return b
}
