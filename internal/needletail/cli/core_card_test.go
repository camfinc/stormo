package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/sched"
)

// A core tool's card says what it does in words and previews the text a person reads first, while
// the payload stays the whole call: nothing the call carries is left off the card.
func TestCoreToolCards(t *testing.T) {
	rt := &runtime{sched: sched.New(sched.Options{Home: t.TempDir()})}
	cases := []struct {
		tool    string
		args    map[string]any
		title   string
		preview string
	}{
		{"mcp_core_msg_send", map[string]any{"to": "ops", "subject": "Daily cut", "body": "Two open items.", "attach": []string{"/shared/group/x.md"}},
			"send a swarm message to ops", "Daily cut\n\nTwo open items."},
		{"mcp_core_msg_send", map[string]any{"to": "unit:sales", "subject": "Now", "body": "b", "priority": "urgent"}, "send a swarm message to unit:sales (urgent)", "Now\n\nb"},
		{"mcp_core_msg_ack", map[string]any{"id": 45}, "mark swarm message 45 as dealt with", ""},
		{"mcp_core_fs_write", map[string]any{"path": "/shared/group/a.md", "content": "hello", "mode": "append"}, "append to the shared file /shared/group/a.md", "hello"},
		{"mcp_core_whatsapp_send", map[string]any{"chat": "+15550100", "text": "hi"}, "send a WhatsApp message to +15550100", "hi"},
		{"mcp_core_cap_start", map[string]any{"capability": "image"}, "queue a job of the capability image", ""},
		{"mcp_core_fs_lock", map[string]any{"path": "/shared/group/a.md"}, "lock /shared/group/a.md in the shared space", ""},
		{"mcp_core_something_new", map[string]any{"x": 1}, "use the core's something_new", ""},
	}
	for _, c := range cases {
		raw, _ := json.Marshal(c.args)
		card, err := rt.describeCall(nil, c.tool, raw)
		if err != nil {
			t.Fatalf("%s: %v", c.tool, err)
		}
		if card.Title != c.title || card.Preview != c.preview {
			t.Errorf("%s: title %q preview %q", c.tool, card.Title, card.Preview)
		}
		if card.PayloadKind != "json" {
			t.Errorf("%s: payload kind %q", c.tool, card.PayloadKind)
		}
		for k := range c.args {
			if !strings.Contains(card.Payload, `"`+k+`"`) {
				t.Errorf("%s: the payload leaves %s off", c.tool, k)
			}
		}
	}
}
