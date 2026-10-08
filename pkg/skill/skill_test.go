package skill

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestDirs(t *testing.T) {
	for _, c := range []struct {
		tool  Tool
		scope Scope
		want  string
	}{
		{Claude, User, "/h/.claude/skills/stormo"}, {Codex, User, "/h/.agents/skills/stormo"},
		{Claude, Project, "/p/.claude/skills/stormo"}, {Codex, Project, "/p/.agents/skills/stormo"},
	} {
		if got, err := Dir(c.tool, c.scope, "/h", "/p"); err != nil || got != c.want {
			t.Errorf("%s/%s: %q %v", c.tool, c.scope, got, err)
		}
	}
	if _, err := Dir("cursor", User, "/h", "/p"); err == nil {
		t.Error("unknown tool accepted")
	}
}

func TestSkillFrontmatter(t *testing.T) {
	files := Files("v1.2.3", "stormo: …")
	body := files["SKILL.md"]
	parts := strings.SplitN(body, "---\n", 3)
	if len(parts) != 3 || parts[0] != "" {
		t.Fatalf("frontmatter must open on line 1:\n%s", body[:80])
	}
	var fm struct{ Name, Description string }
	if err := yaml.Unmarshal([]byte(parts[1]), &fm); err != nil {
		t.Fatal(err)
	}
	if fm.Name != Name || fm.Description == "" || len(fm.Description) > 1536 {
		t.Errorf("name %q, description %d chars", fm.Name, len(fm.Description))
	}
	if !strings.Contains(body, "installed by stormo v1.2.3") {
		t.Error("version marker missing")
	}
	for _, ref := range []string{"references/commands.md", "references/instances.md"} {
		if !strings.Contains(body, "("+ref+")") || files[ref] == "" {
			t.Errorf("%s not linked or empty", ref)
		}
	}
}

func TestInstallReplacesOwnSkillButNotOthers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude", "skills", Name)
	if err := Install(dir, Files("v1", "cmds"), false); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "stale.md"), []byte("old"), 0o644)
	if err := Install(dir, Files("v2", "cmds"), false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); !strings.Contains(string(b), "stormo v2") {
		t.Error("not updated")
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.md")); err == nil {
		t.Error("stale file kept")
	}
	other := filepath.Join(t.TempDir(), "stormo")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, "SKILL.md"), []byte("---\nname: stormo\n---\nsomeone else's\n"), 0o644)
	if err := Install(other, Files("v1", "c"), false); !errors.Is(err, ErrForeign) {
		t.Errorf("foreign skill overwritten: %v", err)
	}
	if _, err := Uninstall(other, false); !errors.Is(err, ErrForeign) {
		t.Errorf("foreign skill removed: %v", err)
	}
	if err := Install(other, Files("v1", "c"), true); err != nil {
		t.Errorf("--force: %v", err)
	}
	if removed, err := Uninstall(dir, false); !removed || err != nil {
		t.Errorf("uninstall: %v %v", removed, err)
	}
	if removed, _ := Uninstall(dir, false); removed {
		t.Error("second uninstall removed something")
	}
}
