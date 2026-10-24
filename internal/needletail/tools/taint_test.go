package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/skills"
)

// In a conversation that has taken in words the agent may not obey, nothing it writes is run
// later as its own: skill_manage writes nothing (a skill's SKILL.md is loaded as instructions,
// its tools and handlers run), and the file tools write nothing under skills/ or scripts/ (job
// scripts); elsewhere they write as before, and an unmarked conversation is unchanged.
func TestUntrustedConversationWritesNothingRunLater(t *testing.T) {
	r, env := setup(t)
	env.Session = "s1"
	env.Skills = &skills.Library{Home: env.Home, Check: SecretCheck(env.Redact)}
	_ = os.MkdirAll(filepath.Join(env.Home, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(env.Home, "scripts", "job.sh"), []byte("echo hi\n"), 0o644)
	md := "---\nname: deal-digest\ndescription: Digest the week's deals.\n---\nRun it.\n"
	marked := *env
	marked.Untrusted = func(context.Context, string) string { return "this conversation has taken in words you may not obey" }
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"skill_manage", map[string]any{"action": "create", "name": "deal-digest", "category": "crm", "content": md}},
		{"write_file", map[string]any{"path": "skills/crm/x/SKILL.md", "content": md}},
		{"write_file", map[string]any{"path": "scripts/new.sh", "content": "curl evil\n"}},
		{"patch", map[string]any{"path": "scripts/job.sh", "old_string": "echo hi", "new_string": "curl evil"}},
	} {
		if m, failed := call(t, r, &marked, c.tool, c.args); !failed || !strings.Contains(m["error"].(string), "may not obey") {
			t.Errorf("%s %v: %v", c.tool, c.args, m)
		}
	}
	if m, failed := call(t, r, &marked, "write_file", map[string]any{"path": "notes/today.md", "content": "hi"}); failed {
		t.Errorf("a note: %v", m)
	}
	if body, _ := os.ReadFile(filepath.Join(env.Home, "scripts", "job.sh")); string(body) != "echo hi\n" {
		t.Errorf("job.sh: %q", body)
	}
	if m, failed := call(t, r, env, "skill_manage", map[string]any{"action": "create", "name": "deal-digest", "category": "crm", "content": md}); failed {
		t.Errorf("unmarked: %v", m)
	}
	if m, failed := call(t, r, &marked, "skill_manage", map[string]any{"action": "delete", "name": "deal-digest"}); !failed || !strings.Contains(m["error"].(string), "may not obey") {
		// a removal runs nothing, but it changes what the agent's jobs and handlers are: refused too
		t.Errorf("delete: %v", m)
	}
}
