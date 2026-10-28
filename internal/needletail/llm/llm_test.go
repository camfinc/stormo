package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// sse writes chunks as an SSE stream, then [DONE] when done.
func sse(w http.ResponseWriter, done bool, chunks ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	for _, c := range chunks {
		fmt.Fprintf(w, "data: %s\n\n", c)
	}
	if done {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

type recorder struct {
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
}

func (r *recorder) add(req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	r.mu.Lock()
	r.bodies = append(r.bodies, m)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.mu.Unlock()
}

func (r *recorder) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func text(s string) string { return fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, s) }

func TestStreamToolCalls(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"other","context_length":5},{"id":"m1","context_length":64000}]}`))
			return
		}
		rec.add(r)
		sse(w, true,
			`{"choices":[{"delta":{"role":"assistant","reasoning_content":"think "}}]}`,
			text("Let me "),
			`{"choices":[{"delta":{"content":"check.","tool_calls":[{"index":0,"id":"call_A","type":"function","function":{"name":"terminal","arguments":"{\"comm"}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_B","function":{"name":"read_file","arguments":""}}]}}]}`,
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"and\":\"ls\"}"}},{"index":1,"function":{"arguments":"{\"path\":\"x\"}"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120}}`)
	}))
	defer srv.Close()
	c := &Client{Endpoints: []Endpoint{{BaseURL: srv.URL + "/v1", Key: "k1", Model: "m1"}}, Reasoning: "high"}
	resp, err := c.NewTurn().Complete(context.Background(), Request{
		Messages: []Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "call_0", Name: "terminal", Arguments: `{"command":"pwd"}`}}},
			{Role: "tool", ToolCallID: "call_0", Content: "/opt/data"},
		},
		Tools: []Tool{{Name: "terminal", Description: "run", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Let me check." || resp.Reasoning != "think " || resp.FinishReason != "tool_calls" || resp.Usage.TotalTokens != 120 {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "call_A" || resp.ToolCalls[0].Arguments != `{"command":"ls"}` || resp.ToolCalls[1].Name != "read_file" || resp.ToolCalls[1].Arguments != `{"path":"x"}` {
		t.Errorf("calls = %+v", resp.ToolCalls)
	}
	b := rec.bodies[0]
	if b["model"] != "m1" || b["stream"] != true || b["reasoning_effort"] != "high" || rec.auth[0] != "Bearer k1" {
		t.Errorf("request = %v %v", b, rec.auth)
	}
	if so, _ := b["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("stream_options = %v", b["stream_options"])
	}
	// the earlier assistant turn goes back verbatim: its call id, null content
	msgs := b["messages"].([]any)
	asst := msgs[2].(map[string]any)
	call := asst["tool_calls"].([]any)[0].(map[string]any)
	if asst["content"] != nil || call["id"] != "call_0" || call["function"].(map[string]any)["arguments"] != `{"command":"pwd"}` {
		t.Errorf("assistant echo = %v", asst)
	}
	if tool := msgs[3].(map[string]any); tool["tool_call_id"] != "call_0" || tool["content"] != "/opt/data" {
		t.Errorf("tool echo = %v", tool)
	}
	if n := c.ContextLength(context.Background(), 0); n != 64000 {
		t.Errorf("context = %d", n)
	}
	if n := (&Client{DefaultContext: 999}).ContextLength(context.Background(), 3); n != 999 {
		t.Errorf("default context = %d", n)
	}
}

// fallbackCase: the primary's answer, and whether the turn should end up on the fallback.
func TestFallback(t *testing.T) {
	cases := map[string]struct {
		primary  func(w http.ResponseWriter)
		fallback bool
		calls    int // primary calls
	}{
		"401": {func(w http.ResponseWriter) {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":"core_login_missing","message":"sign in"}}`))
		}, true, 1},
		"plan limit": {func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"code":"usage_limit_reached","resets_in_seconds":3600}}`))
		}, true, 1},
		"502": {func(w http.ResponseWriter) { w.WriteHeader(502) }, true, 1},
		"in-band error": {func(w http.ResponseWriter) {
			sse(w, false, text("partial "), `{"error":{"code":"upstream_timeout_idle","message":"idle"}}`)
		}, true, 1},
		"no DONE":            {func(w http.ResponseWriter) { sse(w, false, text("cut")) }, true, 1},
		"rate limit then ok": {nil, false, 2},
		"core busy x4":       {func(w http.ResponseWriter) { busy(w) }, true, 4},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			primaryCalls := 0
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				primaryCalls++
				n := primaryCalls
				mu.Unlock()
				if c.primary == nil { // rate limited once, then fine
					if n == 1 {
						w.Header().Set("Retry-After", "7")
						w.WriteHeader(429)
						_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded"}}`))
						return
					}
					sse(w, true, text("from primary"))
					return
				}
				c.primary(w)
			}))
			defer primary.Close()
			cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sse(w, true, text("from cloud"))
			}))
			defer cloud.Close()
			var waits []time.Duration
			cl := &Client{Endpoints: []Endpoint{{BaseURL: primary.URL, Key: "core", Model: "a"}, {BaseURL: cloud.URL, Key: "cloud", Model: "b"}},
				Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}
			turn := cl.NewTurn()
			resp, err := turn.Complete(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}})
			if err != nil {
				t.Fatal(err)
			}
			want := "from primary"
			if c.fallback {
				want = "from cloud"
			}
			if resp.Content != want || primaryCalls != c.calls {
				t.Errorf("content %q (want %q), primary calls %d (want %d), waits %v", resp.Content, want, primaryCalls, c.calls, waits)
			}
			if name == "rate limit then ok" && (len(waits) != 1 || waits[0] != 7*time.Second) {
				t.Errorf("waits = %v", waits)
			}
			// the turn stays where it moved; a new turn starts at the primary
			if c.fallback {
				before := primaryCalls
				if r, _ := turn.Complete(context.Background(), Request{}); r == nil || r.Content != "from cloud" || primaryCalls != before {
					t.Errorf("turn left the fallback")
				}
				if cl.NewTurn().Endpoint() != 0 {
					t.Error("new turn not on primary")
				}
			}
		})
	}
}

func busy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(503)
	_, _ = w.Write([]byte(`{"error":{"type":"server_busy","code":"core_busy"}}`))
}

func TestFatalAndExhausted(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"model and messages are required"}}`))
	}))
	defer bad.Close()
	rec := &recorder{}
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { rec.add(r); w.WriteHeader(500) }))
	defer cloud.Close()
	cl := &Client{Endpoints: []Endpoint{{BaseURL: bad.URL}, {BaseURL: cloud.URL}}}
	_, err := cl.NewTurn().Complete(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "invalid_request") || rec.n() != 0 {
		t.Errorf("400: %v (cloud calls %d)", err, rec.n())
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cl = &Client{Endpoints: []Endpoint{{BaseURL: dead.URL}, {BaseURL: cloud.URL}}}
	_, err = cl.NewTurn().Complete(context.Background(), Request{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || rec.n() != 1 {
		t.Errorf("all failed: %v", err)
	}
	// a stalled stream is a failed attempt, not a hang
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer stall.Close()
	cl = &Client{Endpoints: []Endpoint{{BaseURL: stall.URL}}, IdleTimeout: 100 * time.Millisecond}
	if _, err := cl.NewTurn().Complete(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("stall: %v", err)
	}
}

func TestContextExceeded(t *testing.T) {
	for err, want := range map[error]bool{
		&Failure{Status: 400, Code: "context_length_exceeded"}:                                        true,
		&Failure{Status: 400, Code: "invalid_request_error", Message: "Maximum context length is 8k"}: true,
		&Failure{Status: 413, Message: "prompt exceeds the context window"}:                           true,
		&Failure{Status: 400, Code: "invalid_request_error", Message: "bad tool schema"}:              false,
		&Failure{Status: 500, Code: "context_length_exceeded"}:                                        false,
		errors.New("context length"):                                                                  false,
	} {
		if got := ContextExceeded(err); got != want {
			t.Errorf("%v: %v", err, got)
		}
	}
}

// Providers that leave out the index: a new id starts a new call, and a name repeated across
// chunks is not doubled.
func TestToolCallsWithoutIndex(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"terminal","arguments":"{\"command\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"terminal","arguments":"\"ls\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_2","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_3","function":{"name":"memory","arguments":"{}"}},{"id":"call_4","function":{"name":"terminal","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`, ``,
	}, "\n\n")
	resp, f := readStream(strings.NewReader(body), func() {})
	if f != nil {
		t.Fatal(f)
	}
	got := []string{}
	for _, c := range resp.ToolCalls {
		got = append(got, c.ID+" "+c.Name+" "+c.Arguments)
	}
	want := []string{`call_1 terminal {"command":"ls"}`, `call_2 read_file {"path":"a"}`, `call_3 memory {}`, `call_4 terminal {}`}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls =\n%s", strings.Join(got, "\n"))
	}
}

// A provider that sends calls with no id: each gets one that no other response of the process
// reuses, so the core's call spans and live calls stay apart across a turn's steps.
func TestSynthesizedCallIDsAreUnique(t *testing.T) {
	body := strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"terminal","arguments":"{}"}},{"index":1,"function":{"name":"read_file","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`, ``,
	}, "\n\n")
	seen := map[string]bool{}
	for range 3 {
		resp, f := readStream(strings.NewReader(body), func() {})
		if f != nil {
			t.Fatal(f)
		}
		if len(resp.ToolCalls) != 2 {
			t.Fatalf("calls %+v", resp.ToolCalls)
		}
		for _, c := range resp.ToolCalls {
			if !strings.HasPrefix(c.ID, "call_") || seen[c.ID] {
				t.Errorf("id %q reused or malformed (seen %v)", c.ID, seen)
			}
			seen[c.ID] = true
		}
	}
}

// Serves: the endpoint's GET /models lists the model (the core's gateway lists the plan's).
func TestServes(t *testing.T) {
	var asked int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer core-key" {
			http.Error(w, "no", 401)
			return
		}
		asked++
		fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-6","context_length":400000},{"id":"gpt-6-mini"}]}`)
	}))
	defer srv.Close()
	c := &Client{Endpoints: []Endpoint{{BaseURL: srv.URL, Key: "core-key", Model: "gpt-6"}}}
	if !c.Serves(context.Background(), 0, "gpt-6-mini") || c.Serves(context.Background(), 0, "claude-x") {
		t.Error("serves")
	}
	if asked != 1 {
		t.Errorf("the list was asked %d times (cached)", asked)
	}
	if c.Serves(context.Background(), 5, "gpt-6") {
		t.Error("an endpoint that is not there serves")
	}
	down := &Client{Endpoints: []Endpoint{{BaseURL: "http://127.0.0.1:1", Model: "m"}}}
	if down.Serves(context.Background(), 0, "m") {
		t.Error("an unreachable endpoint serves")
	}
}
