package shared

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestSharedDocsScriptCreatesAndListsDocuments(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	files := Skill(inst, a)
	skill := files["acme-shared-docs/SKILL.md"]
	if !strings.Contains(skill, "/shared/sales") || !strings.Contains(skill, "${HERMES_SKILL_DIR}/scripts/shared_docs.py") || !strings.Contains(skill, "every Acme agent") {
		t.Errorf("skill text:\n%s", skill)
	}
	tmp := t.TempDir()
	script := filepath.Join(tmp, "shared_docs.py")
	os.WriteFile(script, []byte(files["acme-shared-docs/scripts/shared_docs.py"]), 0o755)
	root := filepath.Join(tmp, "shared")
	os.MkdirAll(filepath.Join(root, "group"), 0o755)
	os.MkdirAll(filepath.Join(root, "sales"), 0o755)
	run := func(stdin string, args ...string) (int, string) {
		cmd := exec.Command("python3", append([]string{"-I", script}, args...)...)
		cmd.Env = append(os.Environ(), "SWARM_SHARED_DIR="+root, "SWARM_AGENT=atlas", "SWARM_UNIT=sales")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return code, strings.TrimSpace(string(out))
	}
	ca, a1 := run("Body text.", "new", "sales", "Q4 Plan", "--title", "Q4 plan", "--tags", "planning,brief")
	_, b1 := run("Second.", "new", "sales", "Q4 Plan", "--title", "Q4 plan v2")
	if ca != 0 || a1 == b1 {
		t.Fatalf("new: %d %q %q", ca, a1, b1)
	}
	body, _ := os.ReadFile(a1)
	if !regexp.MustCompile(`^---\ntitle: "Q4 plan"\nauthor: atlas\nunit: sales\ncreated: .+\ntags: \[planning, brief\]\n---\n\nBody text\.\n$`).Match(body) {
		t.Errorf("document:\n%s", body)
	}
	if _, h := run("Need a banner.", "new", "group", "visual-request", "--title", "Banner for Q4", "--to", "nova"); !strings.Contains(h, filepath.Join("group", "handoffs", "nova")) {
		t.Errorf("handoff path %q", h)
	}
	if c, _ := run("", "new", "support", "x", "--title", "nope"); c == 0 {
		t.Error("wrote into another unit's directory")
	}
	if _, list := run("", "list"); len(strings.Split(list, "\n")) != 3 {
		t.Errorf("list:\n%s", list)
	}
	if _, l := run("", "list", "--to", "nova"); !strings.Contains(l, "Banner for Q4") {
		t.Errorf("list --to: %s", l)
	}
}

func TestLayersAreGroupAndOwnUnitOnly(t *testing.T) {
	l := Layers(&manifest.Agent{Unit: "sales"})
	if len(l) != 2 || l[0].Path != "/shared/group" || l[1].Path != "/shared/sales" {
		t.Errorf("layers = %+v", l)
	}
}
