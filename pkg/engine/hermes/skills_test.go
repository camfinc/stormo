package hermes

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenderSkill(t *testing.T) {
	stormo := "---\nname: x\nmetadata:\n  tags: [a, b]\n  related_skills: [y]\n---\nRun `python3 ${SKILL_DIR}/scripts/x.py`.\n"
	want := "---\nname: x\nmetadata:\n  hermes:\n    tags: [a, b]\n    related_skills: [y]\n---\nRun `python3 ${HERMES_SKILL_DIR}/scripts/x.py`.\n"
	if got := renderSkill(stormo); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// Already Hermes', or no metadata, or no frontmatter: unchanged.
	for _, s := range []string{want, "---\nname: x\n---\nbody\n", "no frontmatter\nmetadata:\n  tags: [a]\n"} {
		if got := renderSkill(s); got != s {
			t.Errorf("changed:\n%s\n->\n%s", s, got)
		}
	}
}

func TestEngineFileFallsBackToFormatZero(t *testing.T) {
	dir := t.TempDir()
	if rel, err := engineFile(dir, ConfigFile, "hermes/config.base.yaml"); err != nil || rel != ConfigFile {
		t.Errorf("neither: %q %v", rel, err)
	}
	write := func(rel string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("a: 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hermes/config.base.yaml")
	if rel, err := engineFile(dir, ConfigFile, "hermes/config.base.yaml"); err != nil || rel != "hermes/config.base.yaml" {
		t.Errorf("legacy: %q %v", rel, err)
	}
	write(ConfigFile)
	if _, err := engineFile(dir, ConfigFile, "hermes/config.base.yaml"); err == nil {
		t.Error("both must be refused")
	}
}
