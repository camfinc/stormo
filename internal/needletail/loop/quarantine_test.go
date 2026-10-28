package loop

import (
	"context"
	"encoding/json"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"net/http/httptest"
	"testing"
)

func TestContextQuarantinePersistsAtTurnBoundary(t *testing.T) {
	st := &stub{pick: func(map[string]any) string { return say("Noted.") }}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	if _, err := a.Store.Create(wire.Session{ID: "job", Source: "cron"}); err != nil {
		t.Fatal(err)
	}
	ctx := messaging.WithUntrusted(context.Background(), "agent-made job")
	if _, err := a.Run(ctx, "job", "Run the saved job.", Options{Job: true}); err != nil {
		t.Fatal(err)
	}
	if !a.Store.Untrusted("job") {
		t.Fatal("context-only quarantine did not persist")
	}
	rows, err := a.Store.Messages("job")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || !rows[0].Untrusted {
		t.Fatal("job input row not marked")
	}
}

func TestMarkedToolReadPersistsBeforeNextTool(t *testing.T) {
	st := &stub{pick: func(req map[string]any) string {
		if lastRow(req)["role"] == "user" {
			return callTool("read", "marked_read", `{}`)
		}
		if lastRow(req)["tool_call_id"] == "read" {
			return callTool("write", "write_probe", `{}`)
		}
		return say("Quarantined.")
	}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	if _, err := a.Store.Create(wire.Session{ID: "reader"}); err != nil {
		t.Fatal(err)
	}
	wrote := false
	a.Tools.Add(tools.Tool{Name: "marked_read", Schema: json.RawMessage(`{"type":"object"}`), Run: func(ctx context.Context, e *tools.Env, _ json.RawMessage) (any, error) {
		if err := messaging.MarkUntrusted(ctx, "marked result"); err != nil {
			return nil, err
		}
		if !a.Store.Untrusted(e.Session) {
			t.Fatal("data released before durable mark")
		}
		return "marked data", nil
	}}, tools.Tool{Name: "write_probe", Schema: json.RawMessage(`{"type":"object"}`), Run: func(ctx context.Context, e *tools.Env, _ json.RawMessage) (any, error) {
		if why := tools.UntrustedWhy(ctx, e); why != "" {
			return nil, messaging.Deferred("write", why)
		}
		wrote = true
		return "wrote", nil
	}})
	if _, err := a.Run(context.Background(), "reader", "Read and persist", Options{}); err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("next tool escaped quarantine")
	}
	rows, err := a.Store.Messages("reader")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Role == "tool" && row.ToolCallID == "read" {
			found = row.Untrusted
		}
	}
	if !found {
		t.Fatal("marked tool result row has no provenance")
	}
}
