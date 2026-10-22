package luau

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

func handler(name, events, extra, body string) string {
	return "--[[needletail\nformat: 1\nname: " + name + "\ndescription: a test handler\nevents: [" + events + "]\n" + extra + "]]\n" + body
}

func TestHandlerHeader(t *testing.T) {
	for _, c := range []struct{ src, err string }{
		{handler("h", "on_message", "", "function on_message(ev) end"), ""},
		{handler("h", "on_schedule", "jobs: [daily, \"*\"]\n", "function on_schedule(ev) end"), ""},
		{handler("h", "", "", ""), "a handler names the events"},
		{handler("h", "on_typing", "", ""), `"on_typing" is not an event`},
		{handler("h", "on_schedule", "", ""), "on_schedule needs jobs"},
		{handler("h", "on_message", "jobs: [x]\n", ""), "jobs only goes with on_schedule"},
		{handler("h", "on_message", "args: {type: object}\n", ""), "a handler takes no args"},
	} {
		_, err := ParseHeaderOf([]byte(c.src), KindHandler)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%q: %v, want %q", c.src, err, c.err)
		}
	}
	// events belong to handlers only
	if _, err := ParseHeader([]byte(strings.Replace(goodTool, "timeout: 5s", "timeout: 5s\nevents: [on_message]", 1)), true); err == nil || !strings.Contains(err.Error(), "belong to a handler") {
		t.Fatalf("a tool with events: %v", err)
	}
	h, _ := ParseHeaderOf([]byte(handler("h", "on_schedule, on_message, on_message", "jobs: [daily, a1b2]\n", "")), KindHandler)
	if strings.Join(h.Events, ",") != "on_schedule,on_message" || !h.Serves("x", "daily") || !h.Serves("a1b2", "") || h.Serves("zz", "weekly") || !h.Handles(OnMessage) || h.Handles(BeforeSend) {
		t.Fatalf("header %+v", h)
	}
}

func TestCheckFile(t *testing.T) {
	rt, _ := newRuntime(t)
	ctx := context.Background()
	if err := rt.CheckFile(ctx, "handlers/greet.luau", []byte(handler("greet", "on_message", "", "function on_message(ev) return nil end"))); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"handlers/greet.luau":     `the header's name "other"`,
		"handlers/Bad.luau":       "a handler lives at handlers/<name>.luau",
		"handlers/sub/greet.luau": "a handler lives at",
		"lib/greet.luau":          "a tool (tools/<name>.luau) or an event handler (handlers/<name>.luau)",
	} {
		err := rt.CheckFile(ctx, rel, []byte(handler("other", "on_message", "", "")))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", rel, err, want)
		}
	}
	if err := rt.CheckFile(ctx, "handlers/greet.luau", []byte(handler("greet", "on_message", "", "function on_message(ev) return {"))); err == nil || !strings.Contains(err.Error(), "syntax") {
		t.Fatalf("a handler that does not compile: %v", err)
	}
	if err := rt.CheckFile(ctx, "tools/double.luau", []byte(goodTool)); err != nil {
		t.Fatal(err)
	}
}

func TestParseDecision(t *testing.T) {
	for _, c := range []struct{ event, value, want, err string }{
		{OnMessage, ``, "pass", ""},
		{OnMessage, `null`, "pass", ""},
		{OnMessage, `"silent"`, "silent", ""},
		{OnMessage, `{"action":"pass","context":"vip customer"}`, "pass vip customer", ""},
		{OnMessage, `{"action":"reply","text":"on it"}`, "reply on it", ""},
		{OnMessage, `{"action":"reply"}`, "", "reply needs a text"},
		{OnMessage, `{"action":"route","to":"x"}`, "", `not "route"`},
		{OnMessage, `{"action":"allow"}`, "", `not "allow"`},
		{OnMessage, `{"action":3}`, "", "action must be a string"},
		{OnMessage, `42`, "", "a handler returns nil or a table"},
		{BeforeSend, `{}`, "allow", ""},
		{BeforeSend, `{"action":"rewrite","text":"x"}`, "rewrite x", ""},
		{BeforeSend, `{"action":"block","reason":"pii"}`, "block", ""},
		{BeforeSend, `{"action":"reply","text":"x"}`, "", `not "reply"`},
		{OnSchedule, `{"action":"reply","text":"done"}`, "reply done", ""},
		{OnMessage, `{"action":"pass","context":"` + strings.Repeat("x", MaxHandlerContext+1) + `"}`, "", "context is over"},
	} {
		d, err := ParseDecision(c.event, json.RawMessage(c.value))
		got := strings.TrimSpace(d.Action + " " + d.Text + d.Context)
		if c.err == "" && (err != nil || got != c.want) || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s %s: %q %v, want %q %q", c.event, c.value, got, err, c.want, c.err)
		}
	}
}

type hookLog struct {
	mu     sync.Mutex
	events []string
}

func (h *hookLog) ToolStart(_ string, c wire.ToolCall) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, "start "+c.Name)
}

func (h *hookLog) ToolDone(_ string, c wire.ToolCall, result string, failed bool, _ time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, fmt.Sprintf("done %s %v %s", c.Name, failed, result))
}

// A message's handlers run in order (skill dir, then name) until one replies or stays silent;
// passes add context; a failing, slow or confused handler is skipped (logged and hooked) and the
// event goes on as if it had passed.
func TestDispatchMessage(t *testing.T) {
	rt, _ := newRuntime(t)
	rt.LuauTimeout = 2 * time.Second
	hooks := &hookLog{}
	rt.Observer = func() HookObserver { return hooks }
	var logs []string
	var mu sync.Mutex
	rt.Log = func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() }
	skill(t, rt.Home, "a/first", map[string]string{
		"handlers/note.luau": handler("note", "on_message, on_mention", "", `function on_message(ev) return {action = "pass", context = "author " .. ev.user_name} end
on_mention = on_message`),
		"handlers/crash.luau": handler("crash", "on_message", "", `function on_message(ev) error("boom") end`),
		"handlers/loop.luau":  handler("loop", "on_message", "timeout: 1s\n", `function on_message(ev) while true do end end`),
		"handlers/weird.luau": handler("weird", "on_message", "", `function on_message(ev) return {action = "route"} end`),
		"handlers/none.luau":  handler("none", "on_mention", "", `return {}`),
	})
	skill(t, rt.Home, "b/second", map[string]string{
		"handlers/faq.luau": handler("faq", "on_message", "", `
function on_message(ev)
  if string.find(ev.text, "hours") then return {action = "reply", text = "We open at 9."} end
  return nil
end`),
		"handlers/zlast.luau": handler("zlast", "on_message", "", `function on_message(ev) return "silent" end`),
	})
	ev := func(text string) map[string]any { return map[string]any{"user_name": "Ana", "text": text} }

	v := rt.Dispatch(context.Background(), OnMessage, "s1", ev("what are your hours?"), nil)
	if v.Action != "reply" || v.Text != "We open at 9." || v.By != "second__faq" || strings.Join(v.Context, "|") != "author Ana" {
		t.Fatalf("verdict %+v", v)
	}
	// the faq passes: after (in b/second, after faq) stays silent
	v = rt.Dispatch(context.Background(), OnMessage, "s1", ev("hello"), nil)
	if v.Action != "silent" || v.By != "second__zlast" || v.Ran != 6 {
		t.Fatalf("verdict %+v", v)
	}
	// none handles on_mention but defines no function: skipped; note passes
	v = rt.Dispatch(context.Background(), OnMention, "s1", ev("hi"), nil)
	if v.Action != "pass" || strings.Join(v.Context, "|") != "author Ana" || v.Ran != 2 {
		t.Fatalf("mention verdict %+v", v)
	}
	mu.Lock()
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	for _, want := range []string{"handler first__crash (on_message) skipped: runtime:", "boom", "handler first__loop (on_message) skipped: timeout",
		`handler first__weird (on_message) skipped: on_message returns an action of pass, reply, silent, not "route"`, "handler first__none (on_mention) skipped: entry"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log lacks %q:\n%s", want, joined)
		}
	}
	hooks.mu.Lock()
	h := strings.Join(hooks.events, "\n")
	hooks.mu.Unlock()
	for _, want := range []string{"start luau:first__crash#on_message", "done luau:first__crash#on_message true", "done luau:second__faq#on_message false {\"action\":\"reply\"}"} {
		if !strings.Contains(h, want) {
			t.Errorf("hooks lack %q:\n%s", want, h)
		}
	}
}

// Before a send every handler sees the text the previous ones left; a block stops the message; a
// handler cannot send a message of its own (no recursion), and the caps are declared ∩ granted.
func TestDispatchBeforeSend(t *testing.T) {
	rt, _ := newRuntime(t)
	m := &fakeMessenger{}
	rt.Messenger = m
	rt.Grants = append(rt.Grants, "message:slack:*")
	skill(t, rt.Home, "a/guard", map[string]string{
		"handlers/sign.luau": handler("sign", "before_send", "", `function before_send(ev) return {action = "rewrite", text = ev.text .. " -- bot"} end`),
		"handlers/echo.luau": handler("echo", "before_send", "caps: [message:slack:*]\n", `
function before_send(ev)
  local ok, err = pcall(send_message, "slack", "loop?")
  log("info", tostring(err))
  return {action = "rewrite", text = ev.text .. " [" .. (ok and "sent" or string.match(err, "^%a+")) .. "]"}
end`),
		"handlers/stop.luau": handler("stop", "before_send", "", `function before_send(ev) if string.find(ev.text, "secret") then return {action = "block", reason = "no secrets"} end end`),
	})
	v := rt.Dispatch(context.Background(), BeforeSend, "s", map[string]any{"text": "hello", "kind": "reply"}, nil)
	if v.Action != "rewrite" || v.Text != "hello [refused] -- bot" || len(m.sent) != 0 {
		t.Fatalf("verdict %+v sent %v", v, m.sent)
	}
	v = rt.Dispatch(context.Background(), BeforeSend, "s", map[string]any{"text": "the secret is"}, nil)
	if v.Action != "block" || v.Reason != "no secrets" || v.By != "guard__stop" {
		t.Fatalf("verdict %+v", v)
	}
	// on_message handlers may send (within their caps); the send is marked as theirs
	skill(t, rt.Home, "c/fwd", map[string]string{
		"handlers/fwd.luau": handler("fwd", "on_message", "caps: [message:slack]\n", `function on_message(ev) send_message("slack", "fyi: " .. ev.text) return nil end`),
		"handlers/no.luau":  handler("no", "on_message", "", `function on_message(ev) send_message("slack", "x") return nil end`),
	})
	v = rt.Dispatch(context.Background(), OnMessage, "s", map[string]any{"text": "hi"}, nil)
	if v.Action != "pass" || len(m.sent) != 1 || m.sent[0].Text != "fyi: hi" || m.ctxEvent != OnMessage {
		t.Fatalf("verdict %+v sent %+v (event %q)", v, m.sent, m.ctxEvent)
	}
}

// before_send fails closed: a handler that fails, passes its bound, answers no valid decision
// (a rewrite over MaxHandlerText) or cannot be loaded blocks the message rather than let the
// text it was there to change go out. A message's handlers still fail open.
func TestBeforeSendFailsClosed(t *testing.T) {
	for name, body := range map[string]string{
		"crash":   `function before_send(ev) error("boom") end`,
		"loop":    `function before_send(ev) while true do end end`,
		"weird":   `function before_send(ev) return {action = "route"} end`,
		"long":    `function before_send(ev) return {action = "rewrite", text = string.rep("x", 40001)} end`,
		"nofn":    `return {}`,
		"broken":  `function before_send(ev) return {action = "allow"`,
		"badtype": `function before_send(ev) return {action = "rewrite", text = 7} end`,
	} {
		t.Run(name, func(t *testing.T) {
			rt, _ := newRuntime(t)
			rt.LuauTimeout = 2 * time.Second
			var logs []string
			var mu sync.Mutex
			rt.Log = func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() }
			hooks := &hookLog{}
			rt.Observer = func() HookObserver { return hooks }
			header := ""
			if name == "loop" {
				header = "timeout: 1s\n"
			}
			files := map[string]string{"handlers/" + name + ".luau": handler(name, "before_send", header, body)}
			skill(t, rt.Home, "a/guard", files)
			// the file is written as the terminal could (skill_manage would refuse a broken one)
			v := rt.Dispatch(context.Background(), BeforeSend, "s", map[string]any{"text": "card 4111 1111 1111 1111", "kind": "send"}, nil)
			if v.Action != "block" || v.By != "guard__"+name || !strings.Contains(v.Reason, "failed") || v.Text != "" {
				t.Fatalf("verdict %+v", v)
			}
			mu.Lock()
			joined := strings.Join(logs, "\n")
			mu.Unlock()
			if !strings.Contains(joined, "handler guard__"+name+" (before_send) failed: the message is blocked") {
				t.Errorf("log:\n%s", joined)
			}
		})
	}
	// on_message stays open: the crash is skipped and the event passes
	rt, _ := newRuntime(t)
	skill(t, rt.Home, "a/guard", map[string]string{"handlers/crash.luau": handler("crash", "on_message", "", `function on_message(ev) error("boom") end`)})
	if v := rt.Dispatch(context.Background(), OnMessage, "s", map[string]any{"text": "hi"}, nil); v.Action != "pass" {
		t.Fatalf("on_message verdict %+v", v)
	}
}

// on_schedule runs only the handlers whose jobs name the run's job; luau_test dry-runs a handler
// with a sample event and shows the decision.
func TestHandlerScheduleAndDryRun(t *testing.T) {
	rt, _ := newRuntime(t)
	m := &fakeMessenger{}
	rt.Messenger = m
	rt.Grants = append(rt.Grants, "message:slack")
	skill(t, rt.Home, "ops/report", map[string]string{
		"handlers/daily.luau": handler("daily", "on_schedule", "jobs: [daily report]\ncaps: [message:slack]\n", `
function on_schedule(ev)
  pcall(send_message, "slack", "starting " .. ev.job.name) -- refused (dry_run) in luau_test
  if ev.script_output == "" then return {action = "reply", text = "nothing new for " .. ev.job.id} end
  return {action = "pass", context = "lines: " .. #ev.script_output}
end`),
	})
	e := Events{RT: rt}
	v := e.OnSchedule(context.Background(), "j1", "weekly", map[string]any{"job": map[string]any{"id": "j1", "name": "weekly"}, "script_output": ""})
	if v.Action != "pass" || v.Ran != 0 {
		t.Fatalf("another job's run: %+v", v)
	}
	v = e.OnSchedule(context.Background(), "j2", "daily report", map[string]any{"job": map[string]any{"id": "j2", "name": "daily report"}, "script_output": ""})
	if v.Action != "reply" || v.Text != "nothing new for j2" {
		t.Fatalf("verdict %+v", v)
	}
	lt := rt.TestTool(nil)
	res, err := lt.Run(context.Background(), &tools.Env{Session: "t"}, json.RawMessage(`{"tool": "ops/report/handlers/daily.luau", "args": {"script_output": "a\nb"}}`))
	if err != nil {
		t.Fatal(err)
	}
	got := res.(map[string]any)
	d, _ := got["decision"].(Decision)
	if got["ok"] != true || got["event"] != OnSchedule || d.Action != "pass" || d.Context != "lines: 3" || len(m.sent) != 1 {
		t.Fatalf("luau_test %+v (sent %d)", got, len(m.sent)) // the dry run sent nothing (the one above did)
	}
	if _, err := lt.Run(context.Background(), &tools.Env{}, json.RawMessage(`{"tool": "report__daily", "event": "before_send"}`)); err == nil || !strings.Contains(err.Error(), "does not handle before_send") {
		t.Fatalf("a handler tested for an event it does not handle: %v", err)
	}
	st := rt.HandlerStatus(context.Background(), "ops/report")
	if len(st) != 1 || strings.Join(st[0].Events, ",") != "on_schedule" || st[0].Jobs[0] != "daily report" || strings.Join(st[0].Effective, ",") != "message:slack" {
		t.Fatalf("status %+v", st)
	}
	var _ messaging.Handlers = e
}
