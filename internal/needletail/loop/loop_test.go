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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// stub is an OpenAI-compatible server answering from a script: each request gets the next reply,
// or the reply picked by pick when set.
type stub struct {
	mu       sync.Mutex
	requests []map[string]any
	pick     func(req map[string]any) string
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
	res, err := a.Run(context.Background(), "s1", "say hello", Options{Observer: ev})
	if err != nil {
		t.Fatal(err)
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
	_, _ = a.Store.Create(wire.Session{ID: "conv", Source: "api_server"})
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
}
