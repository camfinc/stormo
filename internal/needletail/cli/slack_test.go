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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// echoModel answers every turn with "Reply to: <the input's last line>", or "[SILENT]" for an
// input that ends in SILENTJOB.
type echoModel struct{}

func (echoModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	lines := strings.Split(strings.TrimSpace(req.Messages[len(req.Messages)-1].Content), "\n")
	reply := "Reply to: " + lines[len(lines)-1]
	if strings.HasSuffix(lines[len(lines)-1], "SILENTJOB") {
		reply = "[SILENT]"
	}
	out, _ := json.Marshal(reply)
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%s},"finish_reason":"stop"}]}`+"\n\n", out)
	fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// M5's done-when (docs/needletail.md §9): the real serve talks to a Slack app in DMs and
// threads and its schedules deliver to the home channel, against slacktest's fake Slack (Web
// API and Socket Mode on the loopback; NEEDLETAIL_SLACK_API_URL may only name one). A live test
// needs a test app's tokens.
func TestServeSlack(t *testing.T) {
	fake := slacktest.New(t)
	model := httptest.NewServer(echoModel{})
	defer model.Close()
	h := home(t, model.URL+"/v1", model.URL+"/v1")
	cfgPath := filepath.Join(h, "needletail.yaml")
	cfg, _ := os.ReadFile(cfgPath)
	if err := os.WriteFile(cfgPath, append(cfg, []byte("channels: [slack]\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	jobsPath := filepath.Join(h, "cron", "jobs.json")
	f, err := sched.ReadFile(jobsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range []struct{ id, prompt, deliver, origin string }{
		{"homejob0001", "Post the morning digest.", "slack", ""},
		{"originjob01", "Follow up in the thread.", "origin", `{"platform":"slack","chat_id":"C0DEVCHAN1","thread_id":"1700000100.000100"}`},
		{"silentjob01", "Nothing new: SILENTJOB", "slack", ""},
		{"agentjob001", "Post it where I may not.", "slack:C0SECRET01", ""},
	} {
		job := sched.NewJob()
		for k, v := range map[string]any{"id": j.id, "name": j.id, "prompt": j.prompt, "skills": []string{}, "no_agent": false,
			"schedule": map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"}, "enabled": true, "state": "scheduled",
			"next_run_at": "2026-01-01T00:00:00Z", "deliver": j.deliver, "repeat": map[string]any{"times": nil, "completed": 0}} {
			job.Set(k, v)
		}
		if j.origin != "" {
			job.Set("origin", json.RawMessage(j.origin))
		}
		if j.id == "agentjob001" { // made by the agent: the messaging policy holds its targets
			job.Set("created_at", "2026-10-01T00:00:00Z")
		}
		f.Jobs = append(f.Jobs, job)
	}
	if err := f.Write(jobsPath, filepath.Join(h, ".needletail-tmp")); err != nil {
		t.Fatal(err)
	}
	setenv(t, h)
	for k, v := range map[string]string{"SLACK_BOT_TOKEN": slacktest.BotToken, "SLACK_APP_TOKEN": slacktest.AppToken,
		"SLACK_ALLOWED_USERS": "U0ALLOWED1", "SLACK_HOME_CHANNEL": "C0HOME0001", "SLACK_ALLOW_BOTS": "mentions", SlackAPIEnv: fake.API()} {
		t.Setenv(k, v)
	}
	port := freePort(t)
	t.Setenv("NEEDLETAIL_API_PORT", strconv.Itoa(port))
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

	// the scheduled jobs: home channel and origin thread posted, the [SILENT] one not
	posts := fake.WaitPosts(2, 15*time.Second)
	byText := map[string]slacktest.Post{}
	for _, p := range posts {
		byText[p.Text] = p
	}
	if p, ok := byText["Reply to: Post the morning digest."]; !ok || p.Channel != "C0HOME0001" || p.Thread != "" {
		t.Errorf("home delivery %+v", posts)
	}
	if p, ok := byText["Reply to: Follow up in the thread."]; !ok || p.Channel != "C0DEVCHAN1" || p.Thread != "1700000100.000100" {
		t.Errorf("origin delivery %+v", posts)
	}

	// a DM, then a channel mention, then a reply in that thread
	fake.Send("e1", slacktest.Event{Channel: "D0DMCHAN01", ChannelType: "im", User: "U0ALLOWED1", Text: "Hello there", TS: "1700000200.000100"})
	posts = fake.WaitPosts(3, 15*time.Second)
	if p := posts[2]; p.Channel != "D0DMCHAN01" || p.Thread != "" || p.Text != "Reply to: Hello there" {
		t.Errorf("DM reply %+v", p)
	}
	fake.Send("e2", slacktest.Event{Type: "app_mention", Channel: "C0DEVCHAN1", ChannelType: "channel", User: "U0ALLOWED1",
		Text: "<@" + slacktest.BotUser + "> what is the status?", TS: "1700000300.000100"})
	posts = fake.WaitPosts(4, 15*time.Second)
	if p := posts[3]; p.Channel != "C0DEVCHAN1" || p.Thread != "1700000300.000100" || p.Text != "Reply to: what is the status?" {
		t.Errorf("mention reply %+v", p)
	}
	fake.Send("e3", slacktest.Event{Channel: "C0DEVCHAN1", ChannelType: "channel", User: "U0ALLOWED1", Text: "and tomorrow?",
		TS: "1700000300.000200", ThreadTS: "1700000300.000100"})
	posts = fake.WaitPosts(5, 15*time.Second)
	if p := posts[4]; p.Thread != "1700000300.000100" || p.Text != "Reply to: and tomorrow?" {
		t.Errorf("thread reply %+v", p)
	}
	time.Sleep(200 * time.Millisecond)
	for _, p := range fake.Posts() {
		if strings.Contains(p.Text, "SILENT") {
			t.Errorf("a [SILENT] run was delivered: %+v", p)
		}
	}

	// the activity shows the gateway, and the jobs' records
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/activity", port), nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var act wire.Activity
	_ = json.NewDecoder(res.Body).Decode(&act)
	res.Body.Close()
	if act.Platforms["slack"].State != "connected" {
		t.Errorf("activity %+v", act)
	}
	jobs := jobsIn(t, h)
	for end := time.Now().Add(15 * time.Second); time.Now().Before(end); jobs = jobsIn(t, h) {
		if jobs["agentjob001"]["last_status"] != nil && jobs["silentjob01"]["last_status"] != nil {
			break // the jobs' lane runs one at a time
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, id := range []string{"homejob0001", "originjob01"} {
		if jobs[id]["last_status"] != "ok" || jobs[id]["last_delivery_error"] != nil {
			t.Errorf("%s: %v / %v", id, jobs[id]["last_status"], jobs[id]["last_delivery_error"])
		}
	}
	if jobs["agentjob001"]["last_status"] != "delivery_failed" || !strings.Contains(fmt.Sprint(jobs["agentjob001"]["last_delivery_error"]), "may message") {
		t.Errorf("an agent job's delivery outside the policy: %v / %v", jobs["agentjob001"]["last_status"], jobs["agentjob001"]["last_delivery_error"])
	}
	if jobs["silentjob01"]["last_status"] != "silent" {
		t.Errorf("silent job: %v", jobs["silentjob01"]["last_status"])
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("serve exit %d", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
	head, err := os.ReadFile(filepath.Join(h, "sessions", "slack-D0DMCHAN01-dm.jsonl"))
	if err != nil || !strings.Contains(strings.SplitN(string(head), "\n", 2)[0], `"source":"slack"`) {
		t.Errorf("DM session: %v %.200s", err, head)
	}
	t.Logf("posts: %d; Web API calls: %d; socket connections: %d; acks: %v", len(fake.Posts()), len(fake.Calls("")), fake.Opens(), fake.Acks())
}

// A job made in a Slack chat delivers back to it (deliver: origin): a thread to its thread, a DM
// to the DM itself (top-level, the one conversation with that person), the API to none.
func TestOriginOfSlackSessions(t *testing.T) {
	store, err := sessions.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []wire.Session{{ID: "slack-C0DEVCHAN1-1700000002.000100", Source: "slack"}, {ID: "slack-D0DMCHAN01-dm", Source: "slack"}, {ID: "api-1", Source: "api_server"}} {
		if _, err := store.Create(s); err != nil {
			t.Fatal(err)
		}
	}
	o := origin(store)
	for id, want := range map[string]string{
		"slack-C0DEVCHAN1-1700000002.000100": `{"chat_id":"C0DEVCHAN1","platform":"slack","thread_id":"1700000002.000100"}`,
		"slack-D0DMCHAN01-dm":                `{"chat_id":"D0DMCHAN01","platform":"slack","thread_id":null}`,
		"api-1":                              ``,
	} {
		if got := string(o(id)); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
}

// The memory's audience of a turn comes from the session's root and source: a thread's channel, a
// DM's person (the gateway's, once known), the owner for a oneshot, none for the API, a job or a
// script. A compaction's child keeps its conversation's.
func TestMemoryAudiences(t *testing.T) {
	store, err := sessions.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []wire.Session{{ID: "slack-C0DEVCHAN1-1700000002.000100", Source: "slack"}, {ID: "slack-D0DMCHAN01-dm", Source: "slack"},
		{ID: "slack-D0DMCHAN02-dm", Source: "slack"}, {ID: "api-1", Source: "api_server"}, {ID: "slack-C9-1.2", Source: "api_server"},
		{ID: "oneshot-1", Source: "cli"}, {ID: "cron-1", Source: "cron"}} {
		if _, err := store.Create(s); err != nil {
			t.Fatal(err)
		}
	}
	child, err := store.Compact("slack-D0DMCHAN01-dm", "summary", nil)
	if err != nil {
		t.Fatal(err)
	}
	a := audiences(store, func(channel string) string {
		if channel == "D0DMCHAN01" {
			return "U0ANA"
		}
		return ""
	})
	for id, want := range map[string]memory.Audience{
		"slack-C0DEVCHAN1-1700000002.000100": {Scope: "channel:C0DEVCHAN1", Conv: "slack-C0DEVCHAN1-1700000002.000100"},
		"slack-D0DMCHAN01-dm":                {Scope: "dm:U0ANA", Conv: "slack-D0DMCHAN01-dm"},
		child:                                {Scope: "dm:U0ANA", Conv: "slack-D0DMCHAN01-dm"},
		"slack-D0DMCHAN02-dm":                {Conv: "slack-D0DMCHAN02-dm"},
		"slack-C0NEW-1700000009.1":           {Scope: "channel:C0NEW", Conv: "slack-C0NEW-1700000009.1"}, // a handler's, before the turn
		"api-1":                              {},
		"slack-C9-1.2":                       {}, // the API's, whatever its name
		"oneshot-1":                          {Operator: true},
		"cron-1":                             {},
		"schedule":                           {},
	} {
		if got := a(id); got != want {
			t.Errorf("%s: %+v, want %+v", id, got, want)
		}
	}
}
