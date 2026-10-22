package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// stub is an OpenAI-compatible server answering from a script: each request gets the next reply,
// or the reply picked by pick when set.
type stub struct {
	mu       sync.Mutex
	requests []map[string]any
	pick     func(req map[string]any) string
	// refuse, when set, answers a request with an HTTP error instead (status 0: no error).
	refuse func(req map[string]any) (int, string)
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/chat/completions" {
		http.NotFound(w, r)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	s.mu.Lock()
	s.requests = append(s.requests, req)
	s.mu.Unlock()
	if s.refuse != nil {
		if code, body := s.refuse(req); code != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			fmt.Fprint(w, body)
			return
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\n", s.pick(req))
	fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func say(text string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}, "finish_reason": "stop"}}})
	return string(b)
}

func callTool(id, name, args string) string {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}},
	}, "finish_reason": "tool_calls"}}})
	return string(b)
}

// lastUser is the newest user or tool row the request carries.
func lastRow(req map[string]any) map[string]any {
	msgs := req["messages"].([]any)
	return msgs[len(msgs)-1].(map[string]any)
}

type events struct{ log []string }

func (e *events) ToolStart(s string, c wire.ToolCall) {
	e.log = append(e.log, "start:"+c.Name+":"+c.ID)
}
func (e *events) ToolDone(s string, c wire.ToolCall, out string, failed bool, _ time.Duration) {
	e.log = append(e.log, fmt.Sprintf("done:%s:%v", c.Name, failed))
}
func (e *events) Text(s, text string) { e.log = append(e.log, "text:"+text) }

// turnEvents also hears turns begin and end (as the core's hooks do).
type turnEvents struct{ events }

func (e *turnEvents) TurnStart(s string) { e.log = append(e.log, "turn:"+s) }
func (e *turnEvents) TurnEnd(s string, err error) {
	e.log = append(e.log, fmt.Sprintf("end:%s:%v", s, err))
}

func newAgent(t *testing.T, srv *httptest.Server) *Agent {
	t.Helper()
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "SOUL.md"), []byte("# Atlas\nYou run deals."), 0o644)
	_ = os.MkdirAll(filepath.Join(home, "skills", "crm", "crm-api"), 0o755)
	_ = os.WriteFile(filepath.Join(home, "skills", "crm", "crm-api", "SKILL.md"), []byte("---\nname: crm-api\ndescription: Query deals.\n---\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(home, "memories"), 0o755)
	_ = os.WriteFile(filepath.Join(home, "memories", "MEMORY.md"), []byte("Deals close on Fridays."), 0o644)
	cfg := &config.Config{Home: home, SharedDir: "/shared", Unit: "sales"}
	cfg.Limits = wire.Limits{Turns: 5, CommandTimeout: 10}
	store, err := sessions.Open(filepath.Join(home, "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mem := &memory.Store{Home: home, Limits: map[string]int{"memory": 2200, "user": 1375}}
	return &Agent{
		Cfg:   cfg,
		LLM:   &llm.Client{Endpoints: []llm.Endpoint{{BaseURL: srv.URL, Key: "k", Model: "m"}}},
		Store: store,
		Tools: tools.NewRegistry(tools.Builtins()...),
		Env:   tools.Env{Home: home, Sandbox: sandbox.Sandbox{Home: home}, CommandTimeout: 10 * time.Second, Memory: mem},
		Now:   func() time.Time { return time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC) },
	}
}

func TestTurnWithTools(t *testing.T) {
	st := &stub{pick: func(req map[string]any) string {
		last := lastRow(req)
		switch {
		case last["role"] == "user":
			return callTool("call_x1", "terminal", `{"command":"echo hello from bash"}`)
		case last["role"] == "tool" && last["tool_call_id"] == "call_x1":
			return callTool("call_x2", "memory", `{"action":"add","target":"user","content":"Likes brevity."}`)
		default:
			return say("Done: hello from bash.")
		}
	}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	if _, err := a.Store.Create(wire.Session{ID: "s1", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	ev := &events{}
	standing := &turnEvents{}
	a.Observer = standing
	res, err := a.Run(context.Background(), "s1", "say hello", Options{Observer: ev})
	if err != nil {
		t.Fatal(err)
	}
	// The agent's own observer hears every turn, start to end, besides the turn's.
	if got := strings.Join(standing.log, ","); got != "turn:s1,start:terminal:call_x1,done:terminal:false,start:memory:call_x2,done:memory:false,text:Done: hello from bash.,end:s1:<nil>" {
		t.Errorf("standing events = %s", got)
	}
	if res.Text != "Done: hello from bash." || res.Session != "s1" || res.Steps != 3 || res.Tokens != 45 {
		t.Errorf("result = %+v", res)
	}
	if got := strings.Join(ev.log, ","); got != "start:terminal:call_x1,done:terminal:false,start:memory:call_x2,done:memory:false,text:Done: hello from bash." {
		t.Errorf("events = %s", got)
	}
	// the system prompt, in order
	sys := st.requests[0]["messages"].([]any)[0].(map[string]any)["content"].(string)
	order := []string{"# Atlas", "MEMORY", "Deals close on Fridays.", "USER PROFILE", "## Skills", "crm-api: Query deals.", "/shared/group", "/shared/sales", "2026-10-20T08:00:00Z"}
	pos := -1
	for _, s := range order {
		i := strings.Index(sys, s)
		if i <= pos {
			t.Errorf("system prompt: %q missing or out of order:\n%s", s, sys)
			break
		}
		pos = i
	}
	// the second request carries the tool result paired with the model's own call id
	tool := lastRow(st.requests[1])
	if tool["tool_call_id"] != "call_x1" || !strings.Contains(tool["content"].(string), "hello from bash") {
		t.Errorf("tool row = %v", tool)
	}
	tr, _ := a.Store.Transcript("s1", 0, false)
	roles := []string{}
	for _, m := range tr.Data {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "user,assistant,tool,assistant,tool,assistant" || tr.Data[2].ToolName != "terminal" {
		t.Errorf("transcript = %v", tr.Data)
	}
	if b, _ := os.ReadFile(filepath.Join(a.Cfg.Home, "memories", "USER.md")); string(b) != "Likes brevity." {
		t.Errorf("USER.md = %q", b)
	}
	if _, err := a.Run(context.Background(), "ghost", "x", Options{}); err != ErrNoSession {
		t.Errorf("ghost: %v", err)
	}
	// A cancelled turn still ends.
	standing.log = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Run(ctx, "s1", "again", Options{}); err == nil {
		t.Error("cancelled turn ran")
	}
	if got := strings.Join(standing.log, ","); got != "turn:s1,end:s1:context canceled" {
		t.Errorf("cancelled turn events = %s", got)
	}
}

func TestTurnLimitAndFilter(t *testing.T) {
	st := &stub{pick: func(req map[string]any) string {
		if req["tools"] == nil {
			return say("Stopping here.")
		}
		return callTool(fmt.Sprintf("call_%d", time.Now().UnixNano()), "write_file", `{"path":"x.txt","content":"x"}`)
	}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	a.Cfg.Limits.Turns = 2
	_, _ = a.Store.Create(wire.Session{ID: "s1"})
	ev := &events{}
	res, err := a.Run(context.Background(), "s1", "loop forever", Options{Tools: []string{"terminal"}, Observer: ev})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Stopping here." || res.Steps != 3 {
		t.Errorf("result = %+v", res)
	}
	// write_file was not offered: refused, never run
	if _, err := os.Stat(filepath.Join(a.Cfg.Home, "x.txt")); !os.IsNotExist(err) {
		t.Error("filtered tool ran")
	}
	if !strings.Contains(strings.Join(ev.log, ","), "done:write_file:true") {
		t.Errorf("events = %v", ev.log)
	}
	if tools := st.requests[0]["tools"].([]any); len(tools) != 1 {
		t.Errorf("offered = %v", tools)
	}
}

func TestCompaction(t *testing.T) {
	st := &stub{pick: func(req map[string]any) string {
		msgs := req["messages"].([]any)
		if strings.HasPrefix(msgs[0].(map[string]any)["content"].(string), "You compact") {
			return say("They discussed deals A and B.")
		}
		last := lastRow(req)
		if last["role"] == "user" && strings.Contains(last["content"].(string), "use a tool") {
			return callTool("call_t", "terminal", `{"command":"echo `+strings.Repeat("y", 400)+`"}`)
		}
		return say("Reply " + strings.Repeat("z", 400))
	}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	a.ContextLength = 1500 // tokens: a few turns fill it
	a.Env.Skills = &skills.Library{Home: a.Cfg.Home}
	_, _ = a.Store.Create(wire.Session{ID: "conv", Source: "api_server"})
	if _, err := a.Env.Skills.View("conv", "crm-api", ""); err != nil {
		t.Fatal(err)
	}
	tip := "conv"
	for i := 0; i < 8; i++ {
		input := fmt.Sprintf("question %d %s", i, strings.Repeat("q", 300))
		if i == 5 {
			input = "please use a tool " + strings.Repeat("q", 300)
		}
		res, err := a.Run(context.Background(), tip, input, Options{})
		if err != nil {
			t.Fatal(err)
		}
		tip = res.Session
	}
	if tip == "conv" {
		t.Fatal("never compacted")
	}
	l, _ := a.Store.Lineage(tip)
	if len(l) < 2 || l[0] != "conv" {
		t.Fatalf("lineage = %v", l)
	}
	if h, _ := a.Store.Get("conv"); h.EndReason != wire.EndCompression {
		t.Errorf("root = %+v", h)
	}
	// the skill the conversation last viewed is still its current one (the terminal's SKILL_DIR)
	if s, ok := a.Env.Skills.LastViewed(tip); !ok || s.Dir != "crm/crm-api" {
		t.Errorf("last viewed after compaction = %+v %v", s, ok)
	}
	// every request is well formed: each tool row answers a call made before it
	for n, req := range st.requests {
		open := map[string]bool{}
		for _, m := range req["messages"].([]any) {
			row := m.(map[string]any)
			if calls, ok := row["tool_calls"].([]any); ok {
				for _, c := range calls {
					open[c.(map[string]any)["id"].(string)] = true
				}
			}
			if row["role"] == "tool" && !open[row["tool_call_id"].(string)] {
				t.Errorf("request %d: orphan tool row %v", n, row["tool_call_id"])
			}
		}
	}
	// the tip resumes with the summary, and the transcript still shows every question once
	rows, _ := a.Store.Messages(tip)
	if rows[0].Role != "system" || !strings.Contains(rows[0].Content, "Summary") {
		t.Errorf("tip starts with %+v", rows[0])
	}
	tr, _ := a.Store.Transcript("conv", 0, false)
	questions := 0
	for _, m := range tr.Data {
		if m.Role == "user" {
			questions++
		}
		if m.Role == "system" {
			t.Error("summary in transcript")
		}
	}
	if questions != 8 || tr.SessionID != tip {
		t.Errorf("transcript: %d questions, tip %s", questions, tr.SessionID)
	}
	// after a restart the carried rows are recognised as copies: nothing is served twice
	again, err := sessions.Open(filepath.Join(a.Cfg.Home, "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr2, _ := again.Transcript("conv", 0, false)
	if len(tr2.Data) != len(tr.Data) {
		t.Errorf("transcript after reopen: %d rows, before %d", len(tr2.Data), len(tr.Data))
	}
	if l := again.List(0); len(l) != 1 || l[0].MessageCount != len(tr.Data) {
		t.Errorf("list after reopen = %+v (want %d messages)", l, len(tr.Data))
	}
}

func TestCutPoint(t *testing.T) {
	rows := []wire.Message{
		{Role: "user", Content: "a"},
		{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "1"}}},
		{Role: "tool", ToolCallID: "1"},
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "c"},
	}
	if c := cutPoint(rows, 1000); c != 1 {
		t.Errorf("roomy cut = %d", c)
	}
	if c := cutPoint(rows, 0); c != 4 {
		t.Errorf("tight cut = %d", c)
	}
	if c := cutPoint(rows[:3], 0); c != 1 {
		t.Errorf("open call cut = %d", c)
	}
	if c := cutPoint([]wire.Message{{Role: "system"}, {Role: "user"}}, 0); c != 0 {
		t.Errorf("nothing to summarise = %d", c)
	}
	// a call left unanswered by a crash does not block every later cut
	orphan := []wire.Message{
		{Role: "user", Content: "a"},
		{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "9"}}},
		{Role: "user", Content: "b"},
		{Role: "assistant", Content: "c"},
		{Role: "user", Content: "d"},
	}
	if c := cutPoint(orphan, 0); c != 4 {
		t.Errorf("orphan tight cut = %d", c)
	}
}

// malformed is why the provider would refuse req's messages with a 400: every assistant row with
// tool calls must be followed by one tool row per call, and a tool row must answer such a call.
func malformed(req map[string]any) string {
	msgs := req["messages"].([]any)
	for i := 0; i < len(msgs); i++ {
		row := msgs[i].(map[string]any)
		if row["role"] == "tool" {
			return fmt.Sprintf("message %d: tool row %v without its call", i, row["tool_call_id"])
		}
		calls, _ := row["tool_calls"].([]any)
		want := map[string]bool{}
		for _, c := range calls {
			want[c.(map[string]any)["id"].(string)] = true
		}
		for len(want) > 0 {
			i++
			if i >= len(msgs) || msgs[i].(map[string]any)["role"] != "tool" {
				return fmt.Sprintf("message %d: tool calls %v unanswered", i, want)
			}
			delete(want, msgs[i].(map[string]any)["tool_call_id"].(string))
		}
	}
	return ""
}

func refuseMalformed(req map[string]any) (int, string) {
	if why := malformed(req); why != "" {
		return 400, fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error"}}`, why)
	}
	return 0, ""
}

// A turn that died between the model's tool calls and their results leaves calls without answers;
// the next turn answers them as interrupted instead of failing on every request from then on.
func TestDanglingToolCalls(t *testing.T) {
	st := &stub{refuse: refuseMalformed, pick: func(req map[string]any) string { return say("ok") }}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	dead := wire.Message{Role: "assistant", ToolCalls: []wire.ToolCall{
		{ID: "call_a", Name: "terminal", Arguments: `{"command":"true"}`},
		{ID: "call_b", Name: "terminal", Arguments: `{"command":"true"}`},
	}}
	// crashed after the first of two results
	_, _ = a.Store.Create(wire.Session{ID: "s1"})
	if _, err := a.Store.Append("s1", wire.Message{Role: "user", Content: "do it"}, dead, wire.Message{Role: "tool", ToolCallID: "call_a", Content: "{}"}); err != nil {
		t.Fatal(err)
	}
	res, err := a.Run(context.Background(), "s1", "and now?", Options{})
	if err != nil || res.Text != "ok" {
		t.Fatalf("run = %+v, %v", res, err)
	}
	rows, _ := a.Store.Messages("s1")
	roles := []string{}
	for _, r := range rows {
		roles = append(roles, r.Role+":"+r.ToolCallID)
	}
	if strings.Join(roles, ",") != "user:,assistant:,tool:call_a,tool:call_b,user:,assistant:" || !strings.Contains(rows[3].Content, "interrupted") {
		t.Errorf("rows = %v", roles)
	}
	// a session already written past such a call (before this fix) is sent well formed too
	_, _ = a.Store.Create(wire.Session{ID: "s2"})
	if _, err := a.Store.Append("s2", wire.Message{Role: "user", Content: "do it"}, dead, wire.Message{Role: "user", Content: "hello?"}); err != nil {
		t.Fatal(err)
	}
	if res, err := a.Run(context.Background(), "s2", "anyone?", Options{}); err != nil || res.Text != "ok" {
		t.Fatalf("legacy run = %+v, %v", res, err)
	}
	// and compaction can cut it
	rows, _ = a.Store.Messages("s2")
	if c := cutPoint(rows, 0); c <= 0 {
		t.Errorf("no cut in %d rows", len(rows))
	}
}

// A provider that refuses a request as too long (the estimate was low) gets one compacted retry.
func TestContextLengthExceeded(t *testing.T) {
	tooLong := `{"error":{"message":"This model's maximum context length is 1000 tokens.","type":"invalid_request_error","code":"context_length_exceeded"}}`
	refuseOld := true
	st := &stub{
		refuse: func(req map[string]any) (int, string) {
			msgs := req["messages"].([]any)
			if strings.HasPrefix(msgs[0].(map[string]any)["content"].(string), "You compact") {
				return 0, ""
			}
			for _, m := range msgs {
				if c, _ := m.(map[string]any)["content"].(string); refuseOld && strings.Contains(c, "OLD-") {
					return 400, tooLong
				}
			}
			return 0, ""
		},
		pick: func(req map[string]any) string {
			if strings.HasPrefix(req["messages"].([]any)[0].(map[string]any)["content"].(string), "You compact") {
				return say("Earlier: old questions.")
			}
			return say("fresh answer")
		},
	}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	a.ContextLength = 1_000_000 // the estimate never asks for a compaction
	_, _ = a.Store.Create(wire.Session{ID: "conv"})
	for i := 0; i < 6; i++ {
		if _, err := a.Store.Append("conv", wire.Message{Role: "user", Content: fmt.Sprintf("OLD-%d question", i)}, wire.Message{Role: "assistant", Content: "old answer"}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := a.Run(context.Background(), "conv", "new question", Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Text != "fresh answer" || res.Session == "conv" {
		t.Errorf("result = %+v", res)
	}
	if h, _ := a.Store.Get("conv"); h.EndReason != wire.EndCompression {
		t.Errorf("not compacted: %+v", h)
	}
	// still too long after the compaction: the error comes back, after one retry only
	refuseOld = false
	_, _ = a.Store.Create(wire.Session{ID: "big"})
	st.refuse = func(req map[string]any) (int, string) {
		if strings.HasPrefix(req["messages"].([]any)[0].(map[string]any)["content"].(string), "You compact") {
			return 0, ""
		}
		return 400, tooLong
	}
	_, _ = a.Store.Append("big", wire.Message{Role: "user", Content: "a"}, wire.Message{Role: "assistant", Content: "b"})
	before := len(st.requests)
	if _, err := a.Run(context.Background(), "big", "c", Options{}); err == nil || !strings.Contains(err.Error(), "context_length_exceeded") {
		t.Errorf("err = %v", err)
	}
	if n := len(st.requests) - before; n != 3 { // the turn, the summary, the retry
		t.Errorf("%d requests", n)
	}
}

// A skill with a huge description, or very many skills, never make the system prompt outgrow the
// context: the index section is bounded and points to skills_list for the rest.
func TestSystemPromptSkillsBounded(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	a := newAgent(t, srv)
	huge := strings.Repeat("deals ", 400)
	for i := 0; i < 300; i++ {
		dir := filepath.Join(a.Cfg.Home, "skills", "many", fmt.Sprintf("s%03d", i))
		_ = os.MkdirAll(dir, 0o755)
		_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(fmt.Sprintf("---\nname: s%03d\ndescription: %s\n---\n", i, huge)), 0o644)
	}
	sys := a.SystemPrompt()
	if len(sys) > 40_000 || !strings.Contains(sys, " more (skills_list)") || !strings.Contains(sys, "crm-api: Query deals.") || !strings.Contains(sys, "## Now") {
		t.Errorf("system prompt is %d bytes", len(sys))
	}
}

// The system prompt's current time is on the agent's wall clock (needletail.yaml's timezone).
func TestSystemPromptTimeZone(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	a := newAgent(t, srv)
	if !strings.Contains(a.SystemPrompt(), "The current time is 2026-10-20T08:00:00Z (Tuesday, UTC).") {
		t.Error("UTC time line changed")
	}
	a.Cfg.Location, _ = time.LoadLocation("America/New_York")
	if sys := a.SystemPrompt(); !strings.Contains(sys, "The current time is 2026-10-20T04:00:00-04:00 (Tuesday, America/New_York).") {
		t.Errorf("system prompt: %s", sys[strings.Index(sys, "## Now"):])
	}
}

// A channel's guidance (the Slack gateway's) joins the system prompt before the time.
func TestSystemPromptSections(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	a := newAgent(t, srv)
	a.Sections = append(a.Sections, func() string { return "## Slack\nStay out of it.\n\n" })
	sys := a.SystemPrompt()
	if i, j := strings.Index(sys, "## Slack\nStay out of it."), strings.Index(sys, "## Now"); i < 0 || j < i {
		t.Errorf("system prompt: %s", sys)
	}
}

// pathEvents records what observers get: names and arguments.
type pathEvents struct{ log []string }

func (e *pathEvents) ToolStart(s string, c wire.ToolCall) {
	e.log = append(e.log, "start:"+c.Name+":"+c.Arguments)
}
func (e *pathEvents) ToolDone(s string, c wire.ToolCall, out string, failed bool, _ time.Duration) {
	e.log = append(e.log, "done:"+c.Name+":"+c.Arguments)
}
func (e *pathEvents) Text(string, string) {}

// Refresh runs before the tools are offered; a tool's hook name is what observers see, and the
// /shared paths it wrote another way are named in tool_done's arguments (the core attributes
// them), while the model still calls it by its own name.
func TestRefreshAndObservedCalls(t *testing.T) {
	st := &stub{}
	n := 0
	st.pick = func(req map[string]any) string {
		n++
		if n == 1 {
			names := []string{}
			for _, x := range req["tools"].([]any) {
				names = append(names, x.(map[string]any)["function"].(map[string]any)["name"].(string))
			}
			if !slices.Contains(names, "crm__quote") {
				t.Errorf("the refreshed tool was not offered: %v", names)
			}
			return callTool("c1", "crm__quote", `{"route":"LIS-JFK"}`)
		}
		return say("done")
	}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	a.Refresh = func(context.Context) {
		a.Tools.SetGroup("luau", []tools.Tool{{Name: "crm__quote", Hook: "luau:crm__quote", Schema: json.RawMessage(`{"type":"object"}`),
			Run: func(_ context.Context, env *tools.Env, _ json.RawMessage) (any, error) {
				env.Touched("/shared/group/a.md")
				env.Touched("/shared/sales/b.md")
				return map[string]any{"ok": true}, nil
			}}})
	}
	h, _ := a.Store.Create(wire.Session{ID: "s1", Source: "api_server"})
	ev := &pathEvents{}
	if _, err := a.Run(context.Background(), h.ID, "quote", Options{Observer: ev}); err != nil {
		t.Fatal(err)
	}
	want := []string{`start:luau:crm__quote:{"route":"LIS-JFK"}`,
		`done:luau:crm__quote:{"path":"/shared/group/a.md","paths":["/shared/group/a.md","/shared/sales/b.md"],"route":"LIS-JFK"}`}
	if !slices.Equal(ev.log, want) {
		t.Fatalf("observed\n%s\nwant\n%s", strings.Join(ev.log, "\n"), strings.Join(want, "\n"))
	}
}
