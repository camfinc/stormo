package hermes

import "testing"

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
