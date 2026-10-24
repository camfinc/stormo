package history

import (
	"context"
	"encoding/json"
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
