package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/mcp"
)

// fakeCore is a core MCP server on loopback: it records tools/call and answers msg_send.
type fakeCore struct {
	mu    sync.Mutex
	calls []map[string]any
	fail  bool
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Content-Type", "application/json")
	switch req["method"] {
	case "initialize":
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"protocolVersion": mcp.ProtocolVersion}})
	case "tools/call":
		f.mu.Lock()
		f.calls = append(f.calls, req["params"].(map[string]any))
		fail := f.fail
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": map[bool]string{false: "Sent: message 7", true: "unknown agent beta"}[fail]}}, "isError": fail}})
	default:
		w.WriteHeader(http.StatusAccepted)
	}
}

// A handler's route to an agent goes over the core's bus as the agent's msg_send; a refusal by the
// core is the route's error.
func TestCoreForward(t *testing.T) {
	core := &fakeCore{}
	srv := httptest.NewServer(core)
	defer srv.Close()
	forward := coreForward(func() *mcp.Client { return mcp.New(srv.URL, "core-key", nil) })
	if err := forward(context.Background(), "atlas", "Forwarded from Slack: hi", "[Forwarded…]\n\nhi"); err != nil {
		t.Fatal(err)
	}
	core.mu.Lock()
	call := core.calls[0]
	core.mu.Unlock()
	args := call["arguments"].(map[string]any)
	if call["name"] != "msg_send" || args["to"] != "atlas" || args["subject"] != "Forwarded from Slack: hi" || !strings.Contains(args["body"].(string), "hi") {
		t.Errorf("call %+v", call)
	}
	core.fail = true
	if err := forward(context.Background(), "beta", "s", "b"); err == nil || !strings.Contains(err.Error(), "unknown agent beta") {
		t.Errorf("refused: %v", err)
	}
	if err := coreForward(func() *mcp.Client { return nil })(context.Background(), "atlas", "s", "b"); err == nil {
		t.Error("forwarded with no core")
	}
}
