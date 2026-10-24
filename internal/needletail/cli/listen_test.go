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

	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
)

// The real serve with slack.listen and a Luau on_message handler that keeps state in kv: an
// overheard message the handler passes on gets a turn (told it was overheard, with the handler's
// note) whose reply goes in its thread; the same text again is silenced by the handler's kv
// dedupe; chatter is silent; a channel not listened to is not heard at all.
func TestServeListen(t *testing.T) {
	fake := slacktest.New(t)
	model := httptest.NewServer(echoModel{})
	defer model.Close()
	h := home(t, model.URL+"/v1", model.URL+"/v1")
	cfgPath := filepath.Join(h, "needletail.yaml")
	cfg, _ := os.ReadFile(cfgPath)
	cfg = []byte(strings.Replace(string(cfg), "grants: [fs.read:home, fs.write:home, core:msg_*]", "grants: [fs.read:home, fs.write:home, core:msg_*, kv:read, kv:write]", 1))
	if err := os.WriteFile(cfgPath, append(cfg, []byte("channels: [slack]\nslack:\n    listen: [C0DEVCHAN1]\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(h, "skills", "ops", "overhear")
	for rel, body := range map[string]string{
		"SKILL.md": "---\nname: overhear\ndescription: Decides which overheard messages matter.\n---\nA handler.\n",
		"handlers/overhear.luau": `--[[needletail
format: 1
name: overhear
description: Passes on booking talk once.
events: [on_message]
caps: [kv:read, kv:write]
]]
function on_message(ev)
  if not ev.ambient then return nil end
  local text = string.lower(ev.text)
  if not string.find(text, "booking", 1, true) then return "silent" end
  local key = "seen:" .. ev.channel .. ":" .. sha256(text)
  if kv_get(key) then return "silent" end
  kv_set(key, true, 12 * 3600)
  return {action = "pass", context = "[OVERHEARD] booking talk"}
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

	fake.Send("e1", slacktest.Event{Channel: "C0DEVCHAN1", ChannelType: "channel", User: "U0ALLOWED1", Text: "The Booking moved to Friday", TS: "1700000800.000100"})
	posts := fake.WaitPosts(1, 15*time.Second)
	if p := posts[0]; p.Channel != "C0DEVCHAN1" || p.Thread != "1700000800.000100" || !strings.Contains(p.Text, "[OVERHEARD] booking talk") {
		t.Errorf("the overheard turn's reply %+v", p)
	}
	fake.Send("e2", slacktest.Event{Channel: "C0DEVCHAN1", ChannelType: "channel", User: "U0ALLOWED1", Text: "the booking moved to friday", TS: "1700000800.000200"})
	fake.Send("e3", slacktest.Event{Channel: "C0DEVCHAN1", ChannelType: "channel", User: "U0ALLOWED1", Text: "lunch?", TS: "1700000800.000300"})
	fake.Send("e4", slacktest.Event{Channel: "C0OTHER001", ChannelType: "channel", User: "U0ALLOWED1", Text: "booking elsewhere", TS: "1700000800.000400"})
	time.Sleep(time.Second)
	if p := fake.Posts(); len(p) != 1 {
		t.Errorf("posts %+v", p)
	}
	if n := len(fake.Calls("reactions.add")); n != 0 {
		t.Errorf("%d reactions on overheard messages", n)
	}
	b, _ := os.ReadFile(filepath.Join(h, "sessions", "slack-C0DEVCHAN1-1700000800.000100.jsonl"))
	if !strings.Contains(string(b), "overheard in a channel you listen to") {
		t.Errorf("the turn's input:\n%s", b)
	}
	for _, ts := range []string{"1700000800.000200", "1700000800.000300"} {
		if _, err := os.Stat(filepath.Join(h, "sessions", "slack-C0DEVCHAN1-"+ts+".jsonl")); err == nil {
			t.Errorf("a silenced message got a session (%s)", ts)
		}
	}
	kv, _ := os.ReadFile(filepath.Join(h, "state", "needletail", "kv.json"))
	if strings.Count(string(kv), `"seen:C0DEVCHAN1:`) != 1 || strings.Contains(string(kv), "friday") {
		t.Errorf("kv.json %s", kv)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop")
	}
}
