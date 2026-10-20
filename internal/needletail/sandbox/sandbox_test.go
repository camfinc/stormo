package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	group := filepath.Join(base, "shared", "group")
	unit := filepath.Join(base, "shared", "sales")
	other := filepath.Join(base, "shared", "ops")
	for _, d := range []string{home, group, unit, other, filepath.Join(home, "skills")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Symlink(other, filepath.Join(home, "to-ops"))
	_ = os.Symlink(filepath.Join(group, "x"), filepath.Join(home, "to-group"))
	_ = os.Symlink("/etc/passwd", filepath.Join(home, "passwd"))
	// planted in the shared space (another agent can write there), pointing into this agent's home
	_ = os.Symlink(filepath.Join(home, "skills"), filepath.Join(group, "x2"))
	_ = os.Symlink(filepath.Join(group, "d2"), filepath.Join(unit, "to-group"))
	s := Sandbox{Home: home, Shared: []string{group, unit}}
	real := func(p string) string { r, _ := evalExisting(p); return r }

	for in, want := range map[string]string{
		"notes.md":                   filepath.Join(home, "notes.md"),
		"skills/new/SKILL.md":        filepath.Join(home, "skills", "new", "SKILL.md"),
		"~/a.txt":                    filepath.Join(home, "a.txt"),
		filepath.Join(group, "d.md"): filepath.Join(group, "d.md"),
		filepath.Join(unit, "u.md"):  filepath.Join(unit, "u.md"),
		"skills/../b.txt":            filepath.Join(home, "b.txt"),
		// the root by its resolved spelling (macOS: /var is /private/var)
		filepath.Join(real(home), "r.md"): filepath.Join(home, "r.md"),
	} {
		got, err := s.Resolve(in)
		if err != nil || got != real(want) {
			t.Errorf("%s: %s %v (want %s)", in, got, err, real(want))
		}
	}
	// a path stays in the root it names: a symlink from one allowed root into another is refused
	// (a write to /shared/group/x2/SKILL.md must not land in the home's skills)
	for _, bad := range []string{"../escape", filepath.Join(other, "o.md"), "to-ops/o.md", "passwd", "/etc/hosts", "", "a\x00b", "skills/../../x",
		"to-group/y", filepath.Join(group, "x2", "SKILL.md"), filepath.Join(group, "x2"), filepath.Join(unit, "to-group", "z")} {
		if got, err := s.Resolve(bad); err == nil {
			t.Errorf("%q resolved to %s", bad, got)
		} else if bad != "" && bad != "a\x00b" && !errors.Is(err, ErrOutside) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	g, _ := s.Resolve(filepath.Join(group, "d.md"))
	h, _ := s.Resolve("notes.md")
	if !s.InShared(g) || s.InShared(h) {
		t.Error("InShared")
	}
	if rel, ok := s.HomeRel(h); !ok || rel != "notes.md" || s.Display(g) != g {
		t.Errorf("HomeRel = %s %v", rel, ok)
	}
}
