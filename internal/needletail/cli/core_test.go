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

	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/pkg/core"
	"github.com/camfinc/stormo/pkg/engine"
	adapter "github.com/camfinc/stormo/pkg/engine/needletail"
)

// coreStub is the swarm core as an agent on it sees it: its MCP server and hook ingest (pkg/core's
// own), keyed for atlas, and a model that sends a message, then writes a shared file.
type coreStub struct {
	mu     sync.Mutex
	calls  []string
	shared string
	db     *core.DB
	mon    *core.Monitor
}

func newCoreStub(t *testing.T, shared string) (*coreStub, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	db, err := core.OpenDB(filepath.Join(dir, "core", "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	keysFile := filepath.Join(dir, "secrets.local.yaml")
	if err := os.WriteFile(keysFile, []byte("local:\n  agents:\n    atlas:\n      SWARM_CORE_KEY: "+coreKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys := core.NewAgentKeys(keysFile)
	c := &coreStub{shared: shared, db: db, mon: core.NewMonitor(db, keys, nil)}
	record := func(name string) core.MCPTool {
		return core.MCPTool{Name: name, Description: name + " (test)", InputSchema: map[string]any{"type": "object"},
			Run: func(tc core.ToolCall) (any, error) {
				c.mu.Lock()
				c.calls = append(c.calls, tc.Agent+":"+name+":"+string(tc.Args))
				c.mu.Unlock()
				return map[string]any{"ok": true}, nil
			}}
	}
	m := core.NewMCP(keys, []core.MCPTool{record("msg_send"), record("fs_note")})
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", m.Handle)
	mux.HandleFunc("/ingest/", c.mon.Handle)
	mux.HandleFunc("/v1/chat/completions", c.model)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return c, srv
}

func (c *coreStub) model(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	last := req.Messages[len(req.Messages)-1]
	w.Header().Set("Content-Type", "text/event-stream")
	call := func(id, name string, args any) {
		a, _ := json.Marshal(args)
		b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{
			map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(a)}}}},
			"finish_reason": "tool_calls"}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	switch {
	case last.Role == "user":
		call("call_m1", "mcp_core_msg_send", map[string]string{"to": "nova", "body": "hello nova"})
	case last.ToolCallID == "call_m1":
		call("call_w1", "write_file", map[string]string{"path": filepath.Join(c.shared, "group", "notes.md"), "content": "from atlas"})
	default:
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"Sent and written."},"finish_reason":"stop"}]}`+"\n\n")
	}
	fmt.Fprint(w, `data: {"choices":[],"usage":{"total_tokens":10}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// On the core: the core's tools are offered as mcp_core_*, a turn's tool calls reach the core's
// hooks with the model's own call ids, and a file-tool write under /shared is noted to the core
// under its shared-space path (M2's done-when, in one process).
func TestServeOnTheCore(t *testing.T) {
	shared := t.TempDir()
	_ = os.MkdirAll(filepath.Join(shared, "group"), 0o755)
	c, srv := newCoreStub(t, shared)
	h := home(t, srv.URL+"/v1", srv.URL+"/v1")
	cfg, _ := os.ReadFile(filepath.Join(h, "needletail.yaml"))
	cfg = bytes.ReplaceAll(cfg, []byte("http://127.0.0.1:1"), []byte(srv.URL))
	_ = os.WriteFile(filepath.Join(h, "needletail.yaml"), cfg, 0o644)
	setenv(t, h)
	t.Setenv("SWARM_SHARED_DIR", shared)
	port := freePort(t)
	t.Setenv("NEEDLETAIL_API_PORT", strconv.Itoa(port))

	var stderr bytes.Buffer
	var mu sync.Mutex
	logs := &lockedWriter{w: &stderr, mu: &mu}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- Run(ctx, []string{"serve"}, io.Discard, logs) }()
	ready := false
	for i := 0; i < 250 && !ready; i++ {
		mu.Lock()
		ready = strings.Contains(stderr.String(), "2 core tools registered")
		mu.Unlock()
		if !ready {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !ready {
		cancel()
		t.Fatalf("core tools never registered: %s", stderr.String())
	}
	call := func(ctx context.Context, method, path string, body any) (int, []byte, error) {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, method, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+apiKey)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		return res.StatusCode, raw, err
	}
	conv := adapter.NewRuntime().Conversations(engine.APICall(call))
	reply, _, err := conv.Send(context.Background(), "chat-core", "tell nova and note it")
	if err != nil || reply != "Sent and written." {
		t.Fatalf("chat = %q %v", reply, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}

	c.mu.Lock()
	calls := strings.Join(c.calls, "\n")
	c.mu.Unlock()
	if calls != `atlas:msg_send:{"body":"hello nova","to":"nova"}`+"\n"+`atlas:fs_note:{"path":"/shared/group/notes.md"}` {
		t.Errorf("core tool calls:\n%s", calls)
	}
	rows, err := c.db.Query(`SELECT event, tool, call, session FROM activity WHERE agent = 'atlas' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var ev, tool, id, sess string
		_ = rows.Scan(&ev, &tool, &id, &sess)
		got = append(got, strings.Trim(ev+" "+tool+" "+id, " ")+" @"+sess)
	}
	want := []string{"session_start @chat-core", "tool_start mcp_core_msg_send call_m1 @chat-core", "tool_done mcp_core_msg_send call_m1 @chat-core",
		"tool_start write_file call_w1 @chat-core", "tool_done write_file call_w1 @chat-core", "session_end @chat-core"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("hooks the core took:\n%s", strings.Join(got, "\n"))
	}
	if l := c.mon.Live("atlas"); l == nil || l.Running != 0 || l.Sessions != 0 || l.LastTool != "write_file" {
		t.Errorf("live %+v", l)
	}
}

// sharedPath maps a resolved path under the shared root to the core's /shared/<layer>/… form.
func TestSharedPath(t *testing.T) {
	root := t.TempDir()
	real, _ := filepath.EvalSymlinks(root)
	f := sharedPath(&config.Config{SharedDir: root})
	for in, want := range map[string]string{
		filepath.Join(real, "group", "a.md"):       "/shared/group/a.md",
		filepath.Join(real, "sales", "x", "b.txt"): "/shared/sales/x/b.txt",
		real: "",
		filepath.Join(filepath.Dir(real), "other"): "",
		"/etc/passwd": "",
	} {
		if got, ok := f(in); got != want || ok != (want != "") {
			t.Errorf("%s: %q %v", in, got, ok)
		}
	}
}
