package cli

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
)

// M6b: the real serve runs the agent's Luau handlers from its skills: on_message answers a
// message with no turn, before_send signs every message the gateway posts (the model's replies,
// the handler's, deliveries) and, when it fails, blocks it, and on_schedule answers a job's run
// with no turn.
func TestServeHandlers(t *testing.T) {
	fake := slacktest.New(t)
	model := httptest.NewServer(echoModel{})
	defer model.Close()
	h := home(t, model.URL+"/v1", model.URL+"/v1")
	cfgPath := filepath.Join(h, "needletail.yaml")
	cfg, _ := os.ReadFile(cfgPath)
	if err := os.WriteFile(cfgPath, append(cfg, []byte("channels: [slack]\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(h, "skills", "ops", "frontdesk")
	for rel, body := range map[string]string{
		"SKILL.md": "---\nname: frontdesk\ndescription: Answers the usual questions.\n---\nHandlers answer opening hours.\n",
		"handlers/hours.luau": `--[[needletail
format: 1
name: hours
description: Answers opening hours without a turn.
events: [on_message, on_mention]
]]
function on_message(ev)
  if string.find(string.lower(ev.text), "hours") then
    return {action = "reply", text = "We open at 9, " .. ev.user_name .. "."}
  end
  return nil
end
on_mention = on_message
`,
		"handlers/sign.luau": `--[[needletail
format: 1
name: sign
description: Signs what goes out.
events: [before_send]
]]
function before_send(ev)
  if string.find(ev.text, "LONG") then
    return {action = "rewrite", text = string.rep("x", 40001)} -- not a valid decision: fails closed
  end
  return {action = "rewrite", text = ev.text .. " [" .. ev.kind .. "]"}
end
`,
		"handlers/daily.luau": `--[[needletail
format: 1
name: daily
description: Answers the daily job.
events: [on_schedule]
jobs: [handlerjob01]
]]
function on_schedule(ev)
  return {action = "reply", text = "Daily from Luau for " .. ev.job.name}
end
`,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(skill, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skill, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	jobsPath := filepath.Join(h, "cron", "jobs.json")
	f, err := sched.ReadFile(jobsPath)
	if err != nil {
		t.Fatal(err)
	}
	job := sched.NewJob()
	for k, v := range map[string]any{"id": "handlerjob01", "name": "morning", "prompt": "Post the digest.", "skills": []string{}, "no_agent": false,
		"schedule": map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"}, "enabled": true, "state": "scheduled",
		"next_run_at": "2026-01-01T00:00:00Z", "deliver": "slack", "repeat": map[string]any{"times": nil, "completed": 0}} {
		job.Set(k, v)
	}
	f.Jobs = append(f.Jobs, job)
	if err := f.Write(jobsPath, filepath.Join(h, ".needletail-tmp")); err != nil {
		t.Fatal(err)
	}
	setenv(t, h)
	for k, v := range map[string]string{"SLACK_BOT_TOKEN": slacktest.BotToken, "SLACK_APP_TOKEN": slacktest.AppToken,
		"SLACK_ALLOWED_USERS": "U0ALLOWED1", "SLACK_HOME_CHANNEL": "C0HOME0001", SlackAPIEnv: fake.API()} {
		t.Setenv(k, v)
	}
	fake.Names["U0ALLOWED1"] = "Alice"
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

	posts := fake.WaitPosts(1, 15*time.Second)
	if posts[0].Channel != "C0HOME0001" || posts[0].Text != "Daily from Luau for morning [delivery]" {
		t.Errorf("the job's delivery %+v", posts[0])
	}
	fake.Send("e1", slacktest.Event{Channel: "D0DMCHAN01", ChannelType: "im", User: "U0ALLOWED1", Text: "What are your hours?", TS: "1700000200.000100"})
	posts = fake.WaitPosts(2, 15*time.Second)
	if p := posts[1]; p.Channel != "D0DMCHAN01" || p.Text != "We open at 9, Alice. [handler_reply]" {
		t.Errorf("the handler's reply %+v", p)
	}
	fake.Send("e2", slacktest.Event{Channel: "D0DMCHAN01", ChannelType: "im", User: "U0ALLOWED1", Text: "Hello there", TS: "1700000200.000200"})
	posts = fake.WaitPosts(3, 15*time.Second)
	if p := posts[2]; p.Text != "Reply to: Hello there [reply]" {
		t.Errorf("the model's reply %+v", p)
	}
	// a before_send handler that fails blocks the reply: nothing goes out unchanged (the next
	// message is sent once the block is logged, so the two are never merged into one turn)
	fake.Send("e3", slacktest.Event{Channel: "D0DMCHAN01", ChannelType: "im", User: "U0ALLOWED1", Text: "Say LONG", TS: "1700000200.000300"})
	blocked := false
	for deadline := time.Now().Add(15 * time.Second); !blocked && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		blocked = strings.Contains(stderr.String(), "handler frontdesk__sign (before_send) failed: the message is blocked")
		mu.Unlock()
	}
	if !blocked {
		t.Error("the failed before_send was not logged")
	}
	fake.Send("e4", slacktest.Event{Channel: "D0DMCHAN01", ChannelType: "im", User: "U0ALLOWED1", Text: "And now?", TS: "1700000200.000400"})
	posts = fake.WaitPosts(4, 15*time.Second)
	if p := posts[3]; p.Text != "Reply to: And now? [reply]" {
		t.Errorf("after the blocked reply %+v", posts[3:])
	}
	entries, _ := os.ReadDir(filepath.Join(h, "sessions"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cron-handlerjob01") {
			t.Errorf("the handled job ran a turn: %s", e.Name())
		}
	}
	if b, _ := os.ReadFile(filepath.Join(h, "sessions", "slack-D0DMCHAN01-dm.jsonl")); strings.Contains(string(b), "What are your hours") {
		t.Error("the message a handler answered reached the model")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
}
