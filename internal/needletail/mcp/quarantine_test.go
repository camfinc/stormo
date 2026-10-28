package mcp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

type quarantineTransport func(*http.Request) (*http.Response, error)

func (f quarantineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestQuarantineCoreWritesRefusedBeforeTransport(t *testing.T) {
	calls := 0
	client := &Client{URL: "http://core.invalid/mcp", Key: "fake-core-key", initialized: true, HTTP: &http.Client{Transport: quarantineTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"result":{"content":[{"type":"text","text":"{}"}]}}`))}, nil
	})}}
	ctx := messaging.WithUntrusted(context.Background(), "unverified words you may not obey")
	for _, name := range []string{"msg_send", "fs_write", "fs_note", "fs_lock", "fs_unlock", "fs_renew", "cap_start", "future_write"} {
		if _, err := client.Call(ctx, name, nil); err == nil || !strings.Contains(err.Error(), "may not obey") {
			t.Errorf("direct %s wasn't quarantined: %v", name, err)
		}
	}
	env := &tools.Env{Untrusted: func(context.Context, string) string { return "unverified words you may not obey" }}
	for _, tool := range client.Tools([]ToolInfo{{Name: "msg_send", InputSchema: []byte(`{}`)}}) {
		if _, err := tool.Run(context.Background(), env, nil); err == nil {
			t.Error("persisted mark bypassed registered MCP tool")
		}
	}
	if calls != 0 {
		t.Fatalf("%d quarantined writes reached core", calls)
	}
	for _, name := range []string{"msg_inbox", "msg_read", "msg_thread", "fs_ls", "fs_info", "fs_locks", "cap_list", "cap_status"} {
		if _, err := client.Call(ctx, name, nil); err != nil {
			t.Errorf("read %s refused: %v", name, err)
		}
	}
	if calls != 8 {
		t.Fatalf("read transport calls %d", calls)
	}
}
