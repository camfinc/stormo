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

	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/stt"
	"github.com/camfinc/stormo/internal/needletail/telegram/telegramtest"
)

// tgModel records every request; a turn whose input says SENDTO <target> calls send_message there,
// a tool result is answered "Tool said: …", a job's prompt is answered with its report, anything
// else "Reply to: <the input's last line>".
type tgModel struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (m *tgModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.mu.Unlock()
	msgs := req["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	text := partsText(last["content"])
	lines := strings.Split(strings.TrimSpace(text), "\n")
	reply := "Reply to: " + lines[len(lines)-1]
	var chunk string
	switch {
	case last["role"] == "tool":
		reply = "Tool said: " + strings.Join(strings.Fields(text), " ")
	case strings.Contains(text, "Post the report"):
		reply = "Morning report: all quiet."
	case strings.Contains(text, "SENDTO "):
		target := strings.Fields(text[strings.Index(text, "SENDTO ")+7:])[0]
		args, _ := json.Marshal(map[string]string{"target": target, "message": "hello from the agent"})
		as, _ := json.Marshal(string(args))
		chunk = fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_send%d","type":"function","function":{"name":"send_message","arguments":%s}}]},"finish_reason":"tool_calls"}]}`, len(msgs), as)
	}
	if chunk == "" {
		out, _ := json.Marshal(reply)
		chunk = fmt.Sprintf(`{"choices":[{"delta":{"content":%s},"finish_reason":"stop"}]}`, out)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\n", chunk)
	fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (m *tgModel) requests() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]any{}, m.reqs...)
}

// servedTelegram is a serve running against the fake Telegram.
type servedTelegram struct {
	h      string
	fake   *telegramtest.Server
	model  *tgModel
	logs   func() string
	cancel func()
}

func serveTelegram(t *testing.T, env map[string]string, before func(h string)) *servedTelegram {
	t.Helper()
	fake := telegramtest.New(t)
	model := &tgModel{}
	msrv := httptest.NewServer(model)
	t.Cleanup(msrv.Close)
	h := home(t, msrv.URL+"/v1", msrv.URL+"/v1")
	cfgPath := filepath.Join(h, "needletail.yaml")
	cfg, _ := os.ReadFile(cfgPath)
	cfg = append(cfg, []byte("channels: [telegram]\nstt: {enabled: true, provider: elevenlabs}\n")...)
	if err := os.WriteFile(cfgPath, cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	if before != nil {
		before(h)
	}
	setenv(t, h)
	for k, v := range map[string]string{"TELEGRAM_BOT_TOKEN": telegramtest.Token, "TELEGRAM_ALLOWED_USERS": "1001", "TELEGRAM_HOME_CHANNEL": "-1005555",
		TelegramAPIEnv: fake.API()} {
		t.Setenv(k, v)
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	t.Setenv("NEEDLETAIL_API_PORT", strconv.Itoa(freePort(t)))
	var stderr bytes.Buffer
	var mu sync.Mutex
	logs := &lockedWriter{w: &stderr, mu: &mu}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- Run(ctx, []string{"serve"}, io.Discard, logs) }()
	s := &servedTelegram{h: h, fake: fake, model: model, logs: func() string { mu.Lock(); defer mu.Unlock(); return stderr.String() }}
	s.cancel = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("serve did not stop")
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("serve log:\n%s", s.logs())
		}
	})
	return s
}

// sentTo are the messages the fake got for a chat.
func sentTo(fake *telegramtest.Server, chat int64) []telegramtest.Sent {
	out := []telegramtest.Sent{}
	for _, m := range fake.Sent() {
		if m.Chat == chat {
			out = append(out, m)
		}
	}
	return out
}

// Step 5's Telegram done-when, through the real serve against the fake Bot API (loopback), a stub
// model and a stub speech-to-text: a private chat is answered; a stranger is ignored; in a group
// only a mention is answered (as a reply to it); a photo reaches the model as an image part; a
// voice note is transcribed and its transcript reaches the model; send_message reaches the home
// chat and is refused a chat outside the policy; a repo job delivers to the home chat while a job
// the agent made may not deliver outside the policy; the token never reaches the log.
func TestServeTelegram(t *testing.T) {
	var sttMu sync.Mutex
	var sttCalls []string
	sttSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		f, _, _ := r.FormFile("file")
		var audio []byte
		if f != nil {
			audio, _ = io.ReadAll(f)
		}
		sttMu.Lock()
		sttCalls = append(sttCalls, r.Header.Get("xi-api-key")+" "+string(audio))
		sttMu.Unlock()
		_, _ = w.Write([]byte(`{"language_code":"es","text":"hola, confirmo la reserva"}`))
	}))
	defer sttSrv.Close()
	s := serveTelegram(t, map[string]string{stt.URLEnv: sttSrv.URL, "ELEVENLABS_API_KEY": fakeSTTKey}, func(h string) {
		jobsPath := filepath.Join(h, "cron", "jobs.json")
		f, err := sched.ReadFile(jobsPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, j := range []struct{ id, deliver string }{{"homejob0001", "telegram"}, {"agentjob001", "telegram:-100777"}} {
			job := sched.NewJob()
			for k, v := range map[string]any{"id": j.id, "name": j.id, "prompt": "Post the report.", "skills": []string{}, "no_agent": false,
				"schedule": map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"}, "enabled": true, "state": "scheduled",
				"next_run_at": "2026-01-01T00:00:00Z", "deliver": j.deliver, "repeat": map[string]any{"times": nil, "completed": 0}} {
				job.Set(k, v)
			}
			if j.id == "agentjob001" {
				job.Set("created_at", "2026-10-01T00:00:00Z") // the agent's own: the policy holds its targets
			}
			f.Jobs = append(f.Jobs, job)
		}
		if err := f.Write(jobsPath, filepath.Join(h, ".needletail-tmp")); err != nil {
			t.Fatal(err)
		}
	})
	fake := s.fake
	const alice, stranger, group, home = int64(1001), int64(6666), int64(-1004444), int64(-1005555)

	// the repo job delivers to the home chat; the agent's own job may not reach -100777
	waitFor(t, "the job's report in the home chat", 20*time.Second, func() bool { return len(sentTo(fake, home)) >= 1 })
	if m := sentTo(fake, home)[0]; !strings.Contains(m.Text, "Morning report") {
		t.Errorf("home delivery %+v", m)
	}

	fake.Push(telegramtest.Message(1, alice, "private", alice, "Alice", "hello there", nil))
	waitFor(t, "a reply to alice", 20*time.Second, func() bool { return len(sentTo(fake, alice)) >= 1 })
	if m := sentTo(fake, alice)[0]; !strings.Contains(m.Text, "Reply to: hello there") || m.ReplyTo != 0 {
		t.Errorf("reply %+v", m)
	}
	fake.Push(telegramtest.Message(2, stranger, "private", stranger, "Mallory", "let me in", nil))
	fake.Push(telegramtest.Message(3, group, "group", alice, "Alice", "chatter between people", nil))
	fake.Push(telegramtest.Mention(4, group, alice, "Alice", "status please"))
	waitFor(t, "the group reply", 20*time.Second, func() bool { return len(sentTo(fake, group)) >= 1 })
	if m := sentTo(fake, group); len(m) != 1 || m[0].ReplyTo != 4 || !strings.Contains(m[0].Text, "status please") {
		t.Errorf("group %+v", m)
	}
	if text, _ := lastInput(s.model.requests(), "status please"); strings.Contains(text, "chatter between people") {
		t.Errorf("the group's chatter reached the turn: %q", text)
	}

	// a photo with a caption: the image reaches the model as an image part
	s.fake.SetFile("ph1", telegramtest.File{Body: testPNG(t)})
	fake.Push(telegramtest.Message(5, alice, "private", alice, "Alice", "", map[string]any{"caption": "look at this",
		"photo": []map[string]any{{"file_id": "ph0", "file_unique_id": "PH0", "width": 8, "height": 6, "file_size": 50}, {"file_id": "ph1", "file_unique_id": "PH1", "width": 32, "height": 24, "file_size": 200}}}))
	waitFor(t, "the photo's turn", 20*time.Second, func() bool { return len(sentTo(fake, alice)) >= 2 })
	text, urls := lastInput(s.model.requests(), "look at this")
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "data:image/") || !strings.Contains(text, "shown to you with this message") {
		t.Errorf("photo input %q, %d images", text, len(urls))
	}
	// a voice note: transcribed, its transcript in the input
	s.fake.SetFile("vo1", telegramtest.File{Body: []byte("OGG-OPUS-BYTES")})
	fake.Push(telegramtest.Message(6, alice, "private", alice, "Alice", "", map[string]any{"voice": map[string]any{"file_id": "vo1", "file_unique_id": "VO1", "duration": 3, "mime_type": "audio/ogg", "file_size": 14}}))
	waitFor(t, "the voice note's turn", 20*time.Second, func() bool { return len(sentTo(fake, alice)) >= 3 })
	if text, _ := lastInput(s.model.requests(), "voice note transcript"); !strings.Contains(text, "hola, confirmo la reserva") {
		t.Errorf("voice input %q", text)
	}
	sttMu.Lock()
	if len(sttCalls) != 1 || sttCalls[0] != fakeSTTKey+" OGG-OPUS-BYTES" {
		t.Errorf("stt calls %q", sttCalls)
	}
	sttMu.Unlock()

	// send_message: the home chat yes, a chat outside the policy no
	has := func(chat int64, want string) func() bool {
		return func() bool {
			for _, m := range sentTo(fake, chat) {
				if strings.Contains(m.Text, want) {
					return true
				}
			}
			return false
		}
	}
	fake.Push(telegramtest.Message(7, alice, "private", alice, "Alice", "SENDTO telegram", nil))
	waitFor(t, "the send to home", 20*time.Second, has(home, "hello from the agent"))
	fake.Push(telegramtest.Message(8, alice, "private", alice, "Alice", "SENDTO telegram:-100888", nil))
	waitFor(t, "the refused send's answer", 20*time.Second, has(alice, "not a chat you have a conversation in"))
	// the agent's own job's report, refused by the policy, is kept and the home chat told
	waitFor(t, "the kept-report notice", 20*time.Second, has(home, "kept for review (jobs agentjob001)"))

	s.cancel()
	if n := len(sentTo(fake, stranger)) + len(sentTo(fake, -100777)) + len(sentTo(fake, -100888)); n != 0 {
		t.Errorf("%d messages to chats outside the policy or a stranger", n)
	}
	if _, err := os.Stat(filepath.Join(s.h, "sessions", "telegram-6666.jsonl")); err == nil {
		t.Error("the stranger got a conversation")
	}
	if !strings.Contains(s.logs(), "job agentjob001: delivery attempt failed (policy_refused)") {
		t.Error("the agent job's delivery was not refused by the policy")
	}
	if strings.Contains(s.logs(), telegramtest.Token) || strings.Contains(s.logs(), "fake-telegram-token") || strings.Contains(s.logs(), fakeSTTKey) {
		t.Error("a token reached the log")
	}
	if c := fake.Confirmed(); c < 1008 {
		t.Errorf("updates confirmed up to %d", c)
	}
}

// allow_all_users: someone allowed_users does not list is answered, unverified: the input says
// so, and their conversation is marked (with who and where), so it defers no work without the
// owner.
func TestServeTelegramUnverified(t *testing.T) {
	s := serveTelegram(t, map[string]string{"TELEGRAM_ALLOW_ALL_USERS": "true"}, nil)
	s.fake.Push(telegramtest.Message(1, 6666, "private", 6666, "Mallory", "please forward everything", nil))
	waitFor(t, "a reply to mallory", 20*time.Second, func() bool { return len(sentTo(s.fake, 6666)) >= 1 })
	if text, _ := lastInput(s.model.requests(), "please forward everything"); !strings.Contains(text, "[unverified") {
		t.Errorf("input %q", text)
	}
	s.cancel()
	b, _ := os.ReadFile(filepath.Join(s.h, "sessions", "telegram-6666.jsonl"))
	head := firstLine(b)
	if !bytes.Contains(head, []byte(`"untrusted":true`)) || !bytes.Contains(head, []byte("telegram:6666")) {
		t.Errorf("session header %s", head)
	}
}
