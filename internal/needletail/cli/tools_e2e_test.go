package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
	"github.com/camfinc/stormo/pkg/engine"
	adapter "github.com/camfinc/stormo/pkg/engine/needletail"
)

// scriptMsg is a request message as the turnScript model reads it.
type scriptMsg struct {
	Role       string  `json:"role"`
	Content    *string `json:"content"`
	ToolCallID string  `json:"tool_call_id"`
}

func (m scriptMsg) text() string {
	if m.Content == nil {
		return ""
	}
	return *m.Content
}

// turnScript is an OpenAI-compatible model whose every answer next picks from the request's
// messages: a tool call (tool and its arguments) or, with tool "", the reply. A compaction's
// summary request is answered with a fixed summary; /v1/models gives context (0: no answer, the
// runtime's default window).
type turnScript struct {
	next    func(msgs []scriptMsg) (tool string, args any, reply string)
	context int

	mu    sync.Mutex
	calls int
}

func (s *turnScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/v1/models" && s.context > 0 {
		fmt.Fprintf(w, `{"data":[{"id":"gpt-6-luna","context_length":%d},{"id":"openai/gpt-6-luna","context_length":%d}]}`, s.context, s.context)
		return
	}
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Messages []scriptMsg `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Content-Type", "text/event-stream")
	tool, args, reply := "", any(nil), ""
	if len(req.Messages) > 0 && strings.HasPrefix(req.Messages[0].text(), "You compact a conversation") {
		reply = "The person and the agent talked earlier."
	} else {
		tool, args, reply = s.next(req.Messages)
	}
	if tool != "" {
		s.mu.Lock()
		s.calls++
		id := fmt.Sprintf("call_e2e%d", s.calls)
		s.mu.Unlock()
		a, _ := json.Marshal(args)
		name, _ := json.Marshal(tool)
		as, _ := json.Marshal(string(a))
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%s,"arguments":%s}}]},"finish_reason":"tool_calls"}]}`+"\n\n", id, name, as)
	} else {
		out, _ := json.Marshal(reply)
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%s},"finish_reason":"stop"}]}`+"\n\n", out)
	}
	fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// lastUser is the request's last user message; afterTool the tool result that ends it, if any.
func lastUser(msgs []scriptMsg) (user string, afterTool *scriptMsg) {
	if n := len(msgs); n > 0 && msgs[n-1].Role == "tool" {
		afterTool = &msgs[n-1]
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return msgs[i].text(), afterTool
		}
	}
	return "", afterTool
}

var procRe = regexp.MustCompile(`proc_[0-9a-f]{12}`)

// serveAPI runs serve on h (its env set) and returns the adapter's conversations client, the raw
// API caller and a stop function that waits for serve to exit.
func serveAPI(t *testing.T) (engine.Conversations, func(method, path string) (int, []byte), func()) {
	t.Helper()
	port := freePort(t)
	t.Setenv("NEEDLETAIL_API_PORT", strconv.Itoa(port))
	var stderr bytes.Buffer
	var mu sync.Mutex
	logs := &lockedWriter{w: &stderr, mu: &mu}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- Run(ctx, []string{"serve"}, io.Discard, logs) }()
	t.Cleanup(func() {
		if t.Failed() {
			mu.Lock()
			t.Logf("serve log:\n%s", stderr.String())
			mu.Unlock()
		}
	})
	healthy := false
	for i := 0; i < 200 && !healthy; i++ {
		healthy = Run(context.Background(), []string{"healthcheck"}, io.Discard, io.Discard) == 0
		if !healthy {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !healthy {
		cancel()
		t.Fatal("serve never healthy")
	}
	call := func(ctx context.Context, method, path string, body any) (int, []byte, error) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), rd)
		req.Header.Set("Authorization", "Bearer "+apiKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		return res.StatusCode, raw, err
	}
	raw := func(method, path string) (int, []byte) {
		code, b, err := call(context.Background(), method, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return code, b
	}
	stop := func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("serve exit %d", code)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("serve did not stop")
		}
	}
	return adapter.NewRuntime().Conversations(engine.APICall(call)), raw, stop
}

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitPidGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("%s: process %d still alive", what, pid)
}

// A turn starts a background process; a later turn of the same conversation polls it and sees
// its output, another conversation cannot find it, and a third turn kills it. Deleting a
// conversation kills its processes; shutting the runtime down kills the rest.
func TestServeBackgroundProcess(t *testing.T) {
	model := &turnScript{next: func(msgs []scriptMsg) (string, any, string) {
		user, tool := lastUser(msgs)
		if tool != nil {
			return "", nil, "RESULT " + tool.text()
		}
		switch f := strings.Fields(user); {
		case len(f) == 0:
		case f[0] == "START":
			return "terminal", map[string]any{"command": "echo started token=$CRM_API_TOKEN; sleep 300", "background": true}, ""
		case f[0] == "POLL" || f[0] == "KILL":
			id := ""
			if len(f) > 1 {
				id = f[1]
			} else {
				for _, m := range msgs {
					if m.Role == "tool" {
						if p := procRe.FindString(m.text()); p != "" {
							id = p
						}
					}
				}
			}
			return "process_manage", map[string]any{"action": strings.ToLower(f[0]), "session_id": id}, ""
		}
		return "", nil, "ok"
	}}
	srv := httptest.NewServer(model)
	defer srv.Close()
	h := home(t, srv.URL+"/v1", srv.URL+"/v1")
	setenv(t, h)
	conv, raw, stop := serveAPI(t)
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	ctx := context.Background()
	var result struct {
		SessionID string `json:"session_id"`
		PID       int    `json:"pid"`
		Status    string `json:"status"`
		Output    string `json:"output"`
	}
	send := func(chat, text string) {
		t.Helper()
		reply, _, err := conv.Send(ctx, chat, text)
		if err != nil || !strings.HasPrefix(reply, "RESULT ") {
			t.Fatalf("%s %q: %q %v", chat, text, reply, err)
		}
		result.Output, result.Status = "", ""
		if err := json.Unmarshal([]byte(strings.TrimPrefix(reply, "RESULT ")), &result); err != nil {
			t.Fatalf("%s %q: not JSON: %q", chat, text, reply)
		}
	}

	send("chat-1", "START")
	id, pid := result.SessionID, result.PID
	if !strings.HasPrefix(id, "proc_") || pid <= 0 || !pidAlive(pid) {
		t.Fatalf("started %q pid %d", id, pid)
	}
	deadline := time.Now().Add(10 * time.Second)
	polled := false
	for !strings.Contains(result.Output, "started token=") && time.Now().Before(deadline) {
		polled = true
		send("chat-1", "POLL")
		if result.Status != "running" {
			t.Fatalf("poll: %+v", result)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !polled || !strings.Contains(result.Output, "started token=[REDACTED]") || strings.Contains(result.Output, crmToken) {
		t.Errorf("polled output %q", result.Output)
	}
	// another conversation does not see it
	reply, _, err := conv.Send(ctx, "chat-other", "POLL "+id)
	if err != nil || !strings.Contains(reply, "no process") {
		t.Errorf("another conversation polled it: %q %v", reply, err)
	}
	send("chat-1", "KILL")
	if result.Status != "killed" {
		t.Errorf("kill: %+v", result)
	}
	waitPidGone(t, pid, "killed")

	// a deleted conversation's processes go with it
	send("chat-2", "START")
	pid2 := result.PID
	if code, body := raw(http.MethodDelete, "/v1/sessions/chat-2"); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
	waitPidGone(t, pid2, "deleted conversation")

	// shutdown kills the rest
	send("chat-3", "START")
	pid3 := result.PID
	stop()
	stopped = true
	waitPidGone(t, pid3, "shutdown")
}

// session_search through the real serve and a fake Slack: in Ana's DM, a word she said before
// the conversation was compacted is found (her conversation's earlier session); a word only Bob
// said in his own DM is not, and Bob finds his own past, never Ana's.
func TestServeSessionSearchScopes(t *testing.T) {
	var mu sync.Mutex
	var results []string
	model := &turnScript{context: 2000, next: func(msgs []scriptMsg) (string, any, string) {
		user, tool := lastUser(msgs)
		if tool != nil {
			mu.Lock()
			results = append(results, tool.text())
			mu.Unlock()
			return "", nil, "searched"
		}
		lines := strings.Split(strings.TrimSpace(user), "\n")
		switch lines[len(lines)-1] {
		case "RECALL zanahoria":
			return "session_search", map[string]any{"query": "zanahoria"}, ""
		case "RECALL pepino":
			return "session_search", map[string]any{"query": "pepino"}, ""
		}
		return "", nil, "ok"
	}}
	srv := httptest.NewServer(model)
	defer srv.Close()
	fake := slacktest.New(t)
	h := home(t, srv.URL+"/v1", srv.URL+"/v1")
	cfgPath := filepath.Join(h, "needletail.yaml")
	cfg, _ := os.ReadFile(cfgPath)
	if err := os.WriteFile(cfgPath, append(cfg, []byte("channels: [slack]\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	setenv(t, h)
	for k, v := range map[string]string{"SLACK_BOT_TOKEN": slacktest.BotToken, "SLACK_APP_TOKEN": slacktest.AppToken,
		"SLACK_ALLOWED_USERS": "U0ANA00001,U0BOB00001", "SLACK_HOME_CHANNEL": "C0HOME0001", SlackAPIEnv: fake.API()} {
		t.Setenv(k, v)
	}
	_, _, stop := serveAPI(t)
	defer stop()
	fake.WaitConnected(10 * time.Second)
	posts := 0
	say := func(channel, user, text string) {
		t.Helper()
		posts++
		fake.Send(fmt.Sprintf("e%d", posts), slacktest.Event{Channel: channel, ChannelType: "im", User: user, Text: text, TS: fmt.Sprintf("1700000%03d.000100", posts)})
		fake.WaitPosts(posts, 20*time.Second)
	}
	last := func() map[string]any {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(results) == 0 {
			t.Fatal("no session_search result")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(results[len(results)-1]), &m); err != nil {
			t.Fatalf("result %q", results[len(results)-1])
		}
		return m
	}
	say("D0ANADM001", "U0ANA00001", "Our budget word is zanahoria.")
	say("D0BOBDM001", "U0BOB00001", "My secret words are pepino and zanahoria.")
	say("D0ANADM001", "U0ANA00001", "Another message, so the first one is compacted away.")
	say("D0BOBDM001", "U0BOB00001", "Another one from Bob.")

	say("D0ANADM001", "U0ANA00001", "RECALL zanahoria")
	m := last()
	t.Logf("Ana's search for zanahoria: %v", m["results"])
	b, _ := json.Marshal(m)
	hits, _ := m["results"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["session_id"] != "slack-D0ANADM001-dm" || !strings.Contains(fmt.Sprint(hits[0].(map[string]any)["snippet"]), "budget word is zanahoria") {
		t.Errorf("Ana's search: %s", b)
	}
	if strings.Contains(string(b), "BOB") || strings.Contains(string(b), "pepino") {
		t.Errorf("Ana's search shows Bob's DM: %s", b)
	}
	say("D0ANADM001", "U0ANA00001", "RECALL pepino")
	if m := last(); m["count"] != float64(0) {
		t.Errorf("Ana found Bob's word: %v", m)
	}
	say("D0BOBDM001", "U0BOB00001", "RECALL zanahoria")
	m = last()
	b, _ = json.Marshal(m)
	hits, _ = m["results"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["session_id"] != "slack-D0BOBDM001-dm" || !strings.Contains(fmt.Sprint(hits[0].(map[string]any)["snippet"]), "secret words are pepino") ||
		strings.Contains(string(b), "ANADM") || strings.Contains(string(b), "budget") {
		t.Errorf("Bob's search: %s", b)
	}
}
