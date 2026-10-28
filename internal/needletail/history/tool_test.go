package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/tools"
)

// The tool picks its shape from the arguments, takes the caller's audience from its session
// (never from the arguments), masks the runtime's secrets through the registry, and its hooks
// carry no query.
func TestTool(t *testing.T) {
	f := newFixture(t)
	f.conv("dm-ana", "slack", "user", "[Slack message from Ana (U1)]\nThe espinaca token is acme_abcdefgh123", "assistant", "Got it.")
	f.conv("dm-bob", "slack", "user", "[Slack message from Bob (U2)]\nespinaca for Bob")
	audience := func(session string) Visible {
		return func(root, _ string) bool { return session == "dm-ana-2" && root == "dm-ana" }
	}
	r := tools.NewRegistry(Tool(f.ix, audience))
	env := &tools.Env{Session: "dm-ana-2", Redact: func(s string) string { return strings.ReplaceAll(s, "acme_abcdefgh123", "[REDACTED]") }}
	run := func(args string) map[string]any {
		t.Helper()
		out, failed := r.Call(context.Background(), env, ToolName, args)
		var m map[string]any
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("%s: %s", args, out)
		}
		if failed {
			m["failed"] = true
		}
		return m
	}
	m := run(`{"query":"espinaca","limit":"5"}`)
	b, _ := json.Marshal(m)
	if m["count"] != float64(1) || !strings.Contains(string(b), "dm-ana") || strings.Contains(string(b), "dm-bob") || strings.Contains(string(b), "acme_abcdefgh123") || !strings.Contains(string(b), "[REDACTED]") {
		t.Errorf("search = %s", b)
	}
	if m := run(`{"session_id":"@session:default/dm-ana","around_message_id":0,"window":1}`); m["mode"] != "scroll" || len(m["messages"].([]any)) != 2 {
		t.Errorf("scroll = %v", m)
	}
	if m := run(`{"session_id":"dm-ana"}`); m["mode"] != "read" {
		t.Errorf("read = %v", m)
	}
	if m := run(`{"session_id":"dm-bob"}`); m["failed"] != true {
		t.Errorf("another person's DM read: %v", m)
	}
	if m := run(`{}`); m["mode"] != "browse" || m["count"] != float64(1) {
		t.Errorf("browse = %v", m)
	}
	// another session of no audience sees nothing
	env.Session = "api-1"
	if m := run(`{"query":"espinaca"}`); m["count"] != float64(0) {
		t.Errorf("no audience found %v", m)
	}
	tool, _ := r.Get(ToolName)
	got := tool.HookArgs(`{"query":"espinaca secreta","session_id":"dm-ana","sort":"newest","limit":3,"exclude_session_ids":["a","b"],"after":"7d"}`)
	if strings.Contains(got, "espinaca") || strings.Contains(got, "dm-ana") || !strings.Contains(got, `"has_query":true`) || !strings.Contains(got, `"sort":"newest"`) || !strings.Contains(got, `"exclude_session_ids_count":2`) {
		t.Errorf("hook args = %s", got)
	}
}

// Marked history must never be returned when no durable reader-mark mechanism exists.
func TestMarkedHistoryFailsClosedWithoutPropagation(t *testing.T) {
	f := newFixture(t)
	f.conv("marked-history", "slack", "user", "quarantine-sentinel secret procedural note")
	path := filepath.Join(f.dir, "marked-history.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"id":"marked-history"`), []byte(`"id":"marked-history","untrusted":true`), 1)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	tool := Tool(f.ix, func(string) Visible { return func(string, string) bool { return true } })
	for _, args := range []string{`{"query":"quarantine-sentinel"}`, `{"session_id":"marked-history"}`, `{"session_id":"marked-history","around_message_id":0}`, `{}`} {
		got, err := tool.Run(context.Background(), &tools.Env{Session: "reader"}, json.RawMessage(args))
		if err == nil || got != nil {
			t.Errorf("marked data returned without durable reader mark: args=%s data=%+v err=%v", args, got, err)
		}
	}
}

func TestMarkedHistoryDurablyQuarantinesBeforeReturn(t *testing.T) {
	f := newFixture(t)
	f.conv("source", "slack", "user", "marked data sentinel")
	if err := f.store.MarkUntrusted("source"); err != nil {
		t.Fatal(err)
	}
	for i, args := range []string{`{"query":"sentinel"}`, `{"session_id":"source"}`, `{"session_id":"source","around_message_id":0}`, `{}`} {
		reader := fmt.Sprintf("reader-%d", i)
		if _, err := f.store.Create(wire.Session{ID: reader, Source: "slack"}); err != nil {
			t.Fatal(err)
		}
		ctx := messaging.WithQuarantine(context.Background(), func(string) error { return f.store.MarkUntrusted(reader) })
		tool := Tool(f.ix, func(string) Visible { return func(string, string) bool { return true } }, f.store.MarkUntrusted)
		got, err := tool.Run(ctx, &tools.Env{Session: reader}, json.RawMessage(args))
		if err != nil || got == nil {
			t.Fatalf("%s: %v", args, err)
		}
		if messaging.UntrustedWhy(ctx) == "" {
			t.Fatal("current run remains trusted")
		}
		reopened, err := sessions.Open(f.dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reopened.Untrusted(reader) {
			t.Fatal("reader mark missing after restart")
		}
	}
	tool := Tool(f.ix, func(string) Visible { return func(string, string) bool { return true } }, func(string) error { return errors.New("forced mark failure") })
	if data, err := tool.Run(context.Background(), &tools.Env{Session: "reader"}, json.RawMessage(`{"session_id":"source"}`)); err == nil || data != nil {
		t.Fatalf("failed persist returned data: %+v %v", data, err)
	}
}
