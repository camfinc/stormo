package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// A card shows exactly what runs: a write over an existing file as a diff, a new file whole, a
// job's change as a diff of its definition, a message as its text.
func TestDescribeCards(t *testing.T) {
	h := home(t, "http://127.0.0.1:1/v1", "http://127.0.0.1:1/v1")
	if err := os.WriteFile(filepath.Join(h, "notes.md"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &runtime{sched: sched.New(sched.Options{Home: h})}
	env := &tools.Env{Home: h, Sandbox: sandbox.Sandbox{Home: h}}
	card := func(tool string, args map[string]any) (string, string, string) {
		b, _ := json.Marshal(args)
		c, err := rt.describe(env, tool, b)
		if err != nil {
			t.Fatal(err)
		}
		return c.Title, c.PayloadKind, c.Payload
	}
	if _, kind, p := card("write_file", map[string]any{"path": "notes.md", "content": "one\nTHREE\n"}); kind != "diff" || !strings.Contains(p, "-two") || !strings.Contains(p, "+THREE") {
		t.Errorf("a write over a file: %s %q", kind, p)
	}
	if _, kind, p := card("write_file", map[string]any{"path": "new.md", "content": "fresh"}); kind != "text" || p != "fresh" {
		t.Errorf("a new file: %s %q", kind, p)
	}
	if _, kind, p := card("patch", map[string]any{"path": "notes.md", "old_string": "two", "new_string": "2"}); kind != "diff" || !strings.Contains(p, "-two") || !strings.Contains(p, "+2") {
		t.Errorf("a patch: %s %q", kind, p)
	}
	if title, kind, p := card(sched.CronJobTool, map[string]any{"action": "update", "job_id": "a1b2c3d4e5f6", "deliver": "slack:C0ELSEWHE1"}); kind != "diff" ||
		!strings.Contains(p, `+  "deliver": "slack:C0ELSEWHE1"`) || !strings.Contains(p, `-  "deliver": "slack"`) || !strings.Contains(title, "a1b2c3d4e5f6") {
		t.Errorf("a job's change: %s %s %q", title, kind, p)
	}
	if title, kind, p := card("send_message", map[string]any{"target": "slack:C0DEVCHAN1", "message": "Hi all"}); kind != "text" || p != "Hi all" || !strings.Contains(title, "slack:C0DEVCHAN1") {
		t.Errorf("a message: %s %s %q", title, kind, p)
	}
	if _, kind, p := card("terminal", map[string]any{"command": "rm -rf ~"}); kind != "command" || p != "rm -rf ~" {
		t.Errorf("a command: %s %q", kind, p)
	}
}
