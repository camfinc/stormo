package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/pkg/core"
)

const agentKey = "atlas-core-key-0123456789abcdef"

// coreMCP is the core's own MCP server (pkg/core) with two test tools, keyed for agent atlas.
func coreMCP(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.local.yaml")
	if err := os.WriteFile(path, []byte("local:\n  agents:\n    atlas:\n      SWARM_CORE_KEY: "+agentKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	calls := []string{}
	m := core.NewMCP(core.NewAgentKeys(path), []core.MCPTool{
		{Name: "msg_send", Description: "Send a message.", InputSchema: map[string]any{"type": "object", "required": []string{"to"},
			"properties": map[string]any{"to": map[string]any{"type": "string"}}},
			Run: func(c core.ToolCall) (any, error) {
				var a struct{ To string }
				_ = json.Unmarshal(c.Args, &a)
				mu.Lock()
				calls = append(calls, c.Agent+"→"+a.To)
				mu.Unlock()
				if a.To == "nobody" {
					return nil, core.ToolError{Msg: "no agent nobody"}
				}
				return map[string]any{"id": 7, "to": a.To}, nil
			}},
		{Name: "fs_note", Description: "Note a change.", InputSchema: map[string]any{"type": "object"},
			Run: func(c core.ToolCall) (any, error) {
				mu.Lock()
				calls = append(calls, "note:"+string(c.Args))
				mu.Unlock()
				return map[string]any{"noted": true}, nil
			}},
	})
	srv := httptest.NewServer(http.HandlerFunc(m.Handle))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, calls...)
	}
}

func logTo(t *testing.T) func(string) { return func(s string) { t.Log(s) } }

// proxyTo forwards requests to target when pass lets them through (pass answers them otherwise).
func proxyTo(target string, pass func(http.ResponseWriter) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !pass(w) {
			return
		}
		req, _ := http.NewRequest(r.Method, target, r.Body)
		req.Header = r.Header
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer res.Body.Close()
		w.Header().Set("Content-Type", res.Header.Get("Content-Type"))
		w.WriteHeader(res.StatusCode)
		_, _ = io.Copy(w, res.Body)
	})
}

// The core's tools join the registry as mcp_core_<name> and run as the agent its key names.
func TestCoreToolsThroughTheCoresTransport(t *testing.T) {
	srv, calls := coreMCP(t)
	c := New(srv.URL, agentKey, logTo(t))
	reg := tools.NewRegistry()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Register(ctx, reg, time.Millisecond, time.Millisecond)
	if got := strings.Join(reg.Names(), ","); got != "mcp_core_fs_note,mcp_core_msg_send" {
		t.Fatalf("registered %s", got)
	}
	tool, _ := reg.Get("mcp_core_msg_send")
	if tool.Description != "Send a message." || !strings.Contains(string(tool.Schema), `"required":["to"]`) {
		t.Errorf("tool %+v %s", tool, tool.Schema)
	}
	out, failed := reg.Call(ctx, &tools.Env{}, "mcp_core_msg_send", `{"to":"nova"}`)
	if failed || out != `{"id":7,"to":"nova"}` {
		t.Errorf("msg_send: %s %v", out, failed)
	}
	// A tool's refusal (isError) is the model's to read, not a failed turn.
	out, failed = reg.Call(ctx, &tools.Env{}, "mcp_core_msg_send", `{"to":"nobody"}`)
	if !failed || out != `{"error":"no agent nobody"}` {
		t.Errorf("refused: %s %v", out, failed)
	}
	if fmt.Sprint(calls()) != "[atlas→nova atlas→nobody]" {
		t.Errorf("calls %v", calls())
	}
	// tools/list is asked once per boot.
	again, err := c.ListTools(ctx)
	if err != nil || len(again) != 2 {
		t.Errorf("cached list: %v %v", again, err)
	}

	// A wrong key is refused by the core, and said so.
	bad := New(srv.URL, "wrong-key-0123456789abcdef", nil)
	if _, err := bad.Call(ctx, "msg_send", json.RawMessage(`{"to":"nova"}`)); err == nil || !strings.Contains(err.Error(), "401") || Away(err) {
		t.Errorf("bad key: %v", err)
	}
}

// relayAwayServer answers as pkg/link's relay does while the core is away: msg_send is queued,
// every other call fails with JSON-RPC -32000.
func relayAwayServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("Authorization") != "Bearer "+agentKey {
			t.Errorf("authorization %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "tools/call" && req.Params.Name == "msg_send" {
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"Queued: the core is unreachable right now."}]}}`, req.ID)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32000,"message":"the core is unreachable right now; try again later"}}`, req.ID)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The core away (the relay standing in, or nothing answering) never fails a turn: the model reads
// why, and the tools are registered once the core lists them.
func TestCoreAway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var hits atomic.Int32
	relay := relayAwayServer(t, &hits)
	c := New(relay.URL, agentKey, logTo(t))
	c.initialized = true // the relay answered initialize before the core left
	reg := tools.NewRegistry(c.Tools([]ToolInfo{{Name: "msg_send"}, {Name: "fs_ls"}})...)
	out, failed := reg.Call(ctx, &tools.Env{}, "mcp_core_msg_send", `{"to":"nova"}`)
	if failed || !strings.Contains(out, "Queued") {
		t.Errorf("queued msg_send: %s %v", out, failed)
	}
	out, failed = reg.Call(ctx, &tools.Env{}, "mcp_core_fs_ls", `{}`)
	if !failed || !strings.Contains(out, "unreachable") {
		t.Errorf("fs_ls away: %s %v", out, failed)
	}
	_, err := c.Call(ctx, "fs_ls", nil)
	if !Away(err) {
		t.Errorf("not away: %v", err)
	}

	// Nothing listening at all.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if _, err := New(dead.URL, agentKey, nil).Call(ctx, "fs_ls", nil); !Away(err) {
		t.Errorf("closed port: %v", err)
	}

	// Away at boot, back later: Register keeps trying, then the tools appear.
	srv, _ := coreMCP(t)
	var fail atomic.Int32
	fail.Store(3)
	flaky := httptest.NewServer(proxyTo(srv.URL, func(w http.ResponseWriter) bool {
		if fail.Add(-1) >= 0 {
			w.WriteHeader(http.StatusBadGateway)
			return false
		}
		return true
	}))
	defer flaky.Close()
	reg2 := tools.NewRegistry()
	New(flaky.URL, agentKey, logTo(t)).Register(ctx, reg2, time.Millisecond, 5*time.Millisecond)
	if len(reg2.Names()) != 2 {
		t.Errorf("after the core came back: %v", reg2.Names())
	}
	// Register gives up only with its context.
	stopped, stop := context.WithCancel(context.Background())
	stop()
	New(dead.URL, agentKey, nil).Register(stopped, tools.NewRegistry(), time.Millisecond, time.Millisecond)
}

// Background calls retry while the core is away, drop what the core refuses, and never block.
func TestBackground(t *testing.T) {
	srv, calls := coreMCP(t)
	var away atomic.Bool
	away.Store(true)
	var refused atomic.Int32
	front := httptest.NewServer(proxyTo(srv.URL, func(w http.ResponseWriter) bool {
		if away.Load() {
			refused.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return false
		}
		return true
	}))
	defer front.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(front.URL, agentKey, logTo(t))
	b := c.Background(ctx, 1)
	b.Wait, b.MaxWait = 5*time.Millisecond, 10*time.Millisecond
	started := time.Now()
	b.Go("fs_note", map[string]string{"path": "/shared/group/a.md"})
	for refused.Load() == 0 && time.Since(started) < 2*time.Second {
		time.Sleep(time.Millisecond) // a.md is being retried, out of the queue
	}
	b.Go("fs_note", map[string]string{"path": "/shared/group/b.md"})
	b.Go("fs_note", map[string]string{"path": "/shared/group/c.md"})
	if time.Since(started) > 2*time.Second {
		t.Error("Go blocked")
	}
	time.Sleep(50 * time.Millisecond)
	away.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(calls()) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	got := strings.Join(calls(), " ")
	// a.md was being retried when c.md arrived and b.md (the oldest queued) was dropped.
	if !strings.Contains(got, "a.md") || !strings.Contains(got, "c.md") || strings.Contains(got, "b.md") {
		t.Errorf("calls %s", got)
	}
	cancel()
	select {
	case <-b.Done():
	case <-time.After(2 * time.Second):
		t.Error("worker did not stop")
	}
	if !errors.Is(fmt.Errorf("x: %w", ErrAway), ErrAway) {
		t.Error("ErrAway wraps")
	}
}

// A core slower than the call's deadline is away for a background call: it is retried, not
// dropped.
func TestBackgroundRetriesASlowCore(t *testing.T) {
	srv, calls := coreMCP(t)
	var slow atomic.Int32
	slow.Store(2)
	front := httptest.NewServer(proxyTo(srv.URL, func(w http.ResponseWriter) bool {
		if slow.Add(-1) >= 0 {
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusServiceUnavailable)
			return false
		}
		return true
	}))
	defer front.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := New(front.URL, agentKey, logTo(t))
	b := c.Background(ctx, 4)
	b.Timeout, b.Wait, b.MaxWait = 50*time.Millisecond, 5*time.Millisecond, 10*time.Millisecond
	b.Go("fs_note", map[string]string{"path": "/shared/group/slow.md"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(calls()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if got := strings.Join(calls(), " "); !strings.Contains(got, "slow.md") {
		t.Errorf("the note was dropped: calls %q", got)
	}
}
