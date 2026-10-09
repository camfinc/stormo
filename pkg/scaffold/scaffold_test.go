package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// The example as the binary embeds it, read from the repository for the test.
func exampleFS(t *testing.T) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	root := filepath.Join("..", "..", "examples", "minimal")
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		rel, _ := filepath.Rel(root, p)
		m[filepath.ToSlash(rel)] = &fstest.MapFile{Data: b}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSlugFrom(t *testing.T) {
	for in, want := range map[string]string{"Acme Swarm": "acme-swarm", "  Zeta & Co.  ": "zeta-co", "42 Labs": "labs", "日本": "", "": ""} {
		if got := SlugFrom(in); got != want {
			t.Errorf("SlugFrom(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmptyInstanceLoadsAndIgnoresLocalFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Application Support", "Stormo", "Instances", "zeta")
	r, err := NewInstance(dir, Options{Name: "Zeta: The Swarm", Org: "Zeta", Git: false}, exampleFS(t))
	if err != nil {
		t.Fatal(err)
	}
	if r.Slug != "zeta-the-swarm" || r.Template != Empty || r.Root != dir {
		t.Errorf("%+v", r)
	}
	inst, err := instance.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Name != "Zeta: The Swarm" || inst.Org != "Zeta" || inst.Slug != "zeta-the-swarm" {
		t.Errorf("%+v", inst)
	}
	if ids := manifest.UnitIDs(dir); len(ids) != 1 || ids[0] != "group" {
		t.Errorf("units %v", ids)
	}
	gi, _ := os.ReadFile(filepath.Join(dir, ".gitignore"))
	for _, p := range []string{"dist/", ".swarm/", "workdir/", "secrets.local.yaml"} {
		if !strings.Contains(string(gi), p) {
			t.Errorf(".gitignore lacks %s", p)
		}
	}
}

func TestExampleTakesTheNewName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo")
	if _, err := NewInstance(dir, Options{Name: "Demo Swarm", Slug: "demo", Template: Example}, exampleFS(t)); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Name != "Demo Swarm" || inst.Slug != "demo" || inst.Org != "Demo Swarm" {
		t.Errorf("%+v", inst)
	}
	if ids := manifest.AgentIDs(dir); len(ids) != 3 {
		t.Errorf("agents %v", ids)
	}
	body, _ := os.ReadFile(filepath.Join(dir, "stormo.yaml"))
	if !strings.Contains(string(body), "# Example Stormo instance") {
		t.Error("the example's comments are gone")
	}
}

func TestRefusals(t *testing.T) {
	base := t.TempDir()
	full := filepath.Join(base, "full")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(full, "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		dir string
		o   Options
	}{
		"no name":      {filepath.Join(base, "a"), Options{}},
		"bad slug":     {filepath.Join(base, "b"), Options{Name: "B", Slug: "B!"}},
		"bad template": {filepath.Join(base, "c"), Options{Name: "C", Template: "huge"}},
		"not empty":    {full, Options{Name: "Full"}},
	}
	for name, c := range cases {
		if _, err := NewInstance(c.dir, c.o, exampleFS(t)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(full, "notes.txt")); string(b) != "mine" {
		t.Error("a refused folder was touched")
	}
	for _, d := range []string{"a", "b", "c"} {
		if _, err := os.Stat(filepath.Join(base, d)); err == nil {
			t.Errorf("%s was created", d)
		}
	}
}

func TestGitInit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := filepath.Join(t.TempDir(), "g")
	r, err := NewInstance(dir, Options{Name: "G", Git: true}, exampleFS(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil || !r.Git {
		t.Errorf("git %v %v", r.Git, err)
	}
}
