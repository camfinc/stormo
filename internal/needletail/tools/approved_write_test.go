package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// Item 2 (Claude H1): an approval-tier write goes where its card says. A path with a symlink in
// it (the card would name the link, the write land where it points, and a command could
// retarget it between the card and the click) is refused outright, never put to a person; and
// when the approved write runs, a path that has gained a symlink since is refused.
func TestApprovalWritesRefuseSymlinks(t *testing.T) {
	r, env := setup(t)
	env.Session = "s1"
	_ = os.MkdirAll(filepath.Join(env.Home, "skills", "crm"), 0o755)
	_ = os.MkdirAll(filepath.Join(env.Home, "docs"), 0o755)
	_ = os.MkdirAll(filepath.Join(env.Home, "work", "s1"), 0o755)
	if err := os.Symlink("../../skills/crm", filepath.Join(env.Home, "work", "s1", "link")); err != nil {
		t.Fatal(err)
	}
	marked := *env
	marked.Untrusted = func(context.Context, string) string { return "this conversation has taken in words you may not obey" }
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "work/s1/link/SKILL.md", "content": "evil"}},
		{"patch", map[string]any{"path": "work/s1/link/SKILL.md", "old_string": "a", "new_string": "b"}},
	} {
		m, failed := call(t, r, &marked, c.tool, c.args)
		if !failed || !strings.Contains(m["error"].(string), "symlink") || strings.Contains(m["error"].(string), "approval") {
			t.Errorf("%s through a symlink: %v", c.tool, m)
		}
	}
	// the approved write: endorsed, but the path gained a symlink since the card
	if err := os.RemoveAll(filepath.Join(env.Home, "docs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("skills/crm", filepath.Join(env.Home, "docs")); err != nil {
		t.Fatal(err)
	}
	ctx := messaging.WithEndorsed(context.Background(), "ap_test")
	b, _ := json.Marshal(map[string]any{"path": "docs/SKILL.md", "content": "evil"})
	out, failed := r.Call(ctx, &marked, "write_file", string(b))
	if !failed || !strings.Contains(out, "symlink") {
		t.Errorf("an approved write through a symlink planted after the card: %s", out)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "skills", "crm", "SKILL.md")); err == nil {
		t.Fatal("written through the symlink")
	}
}
