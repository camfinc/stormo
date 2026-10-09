package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
)

// acme is a scratch copy of the example instance, so writes never touch examples/minimal.
func acme(t *testing.T) *instance.Instance {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("../../examples/minimal")); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func onDisk(t *testing.T, inst *instance.Instance, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(inst.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShowAndWriteKeepComments(t *testing.T) {
	inst := acme(t)
	f, err := Show(inst, "agents/atlas/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "agent" || f.Agent != "atlas" || f.Hash != Hash([]byte(f.Text)) || !strings.Contains(f.Text, "#") {
		t.Fatalf("%+v", f)
	}
	edited := strings.Replace(f.Text, "\nrole: ", "\n# a comment the editor added\nrole: ", 1)
	w, err := Write(inst, f.Path, []byte(edited), f.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got := onDisk(t, inst, f.Path); got != edited || w.Hash != Hash([]byte(edited)) {
		t.Errorf("written %q, hash %s", got, w.Hash)
	}
	// The old hash is now stale.
	if _, err := Write(inst, f.Path, []byte(f.Text), f.Hash); code(err) != "conflict" {
		t.Errorf("stale hash: %v", err)
	}
	if got := onDisk(t, inst, f.Path); got != edited {
		t.Error("a refused write changed the file")
	}
	if entries, _ := os.ReadDir(filepath.Join(inst.Root, "agents/atlas")); len(entries) == 0 {
		t.Fatal("no files")
	} else {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".agent.yaml.") {
				t.Errorf("temporary file left behind: %s", e.Name())
			}
		}
	}
}

func TestWriteRefusesWhatWouldNotLoad(t *testing.T) {
	inst := acme(t)
	f, _ := Show(inst, "agents/atlas/agent.yaml")
	cases := map[string]struct{ body, code string }{
		"bad yaml":         {"id: atlas\nname: [unclosed\n", "invalid"},
		"unknown unit":     {strings.Replace(f.Text, "unit: sales", "unit: nowhere", 1), "invalid"},
		"secret in env":    {f.Text + "\nenv:\n  API_TOKEN: x\n", "invalid"},
		"foreign action":   {strings.Replace(f.Text, "actions:\n  - sales.crm.deals.list", "actions:\n  - support.tickets.list", 1), "invalid"},
		"deploy edited":    {strings.Replace(f.Text, "cpu: 1024", "cpu: 2048", 1), "readonly"},
		"no if-hash given": {f.Text, "usage"},
	}
	for name, c := range cases {
		if c.body == f.Text && name != "no if-hash given" {
			t.Fatalf("%s: the fixture changed, the case edits nothing", name)
		}
		hash := f.Hash
		if c.code == "usage" {
			hash = ""
		}
		if _, err := Write(inst, f.Path, []byte(c.body), hash); code(err) != c.code {
			t.Errorf("%s: want %s, got %v", name, c.code, err)
		}
		if onDisk(t, inst, f.Path) != f.Text {
			t.Fatalf("%s: a refused write changed the file", name)
		}
	}
}

func TestSoul(t *testing.T) {
	inst := acme(t)
	f, err := Show(inst, "agents/atlas/SOUL.md")
	if err != nil || f.Kind != "soul" {
		t.Fatal(f, err)
	}
	if _, err := Write(inst, f.Path, []byte(" \n"), f.Hash); code(err) != "invalid" {
		t.Errorf("empty soul: %v", err)
	}
	if _, err := Write(inst, f.Path, []byte(f.Text+"\nBe brief.\n"), f.Hash); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyAgentFiles(t *testing.T) {
	inst := acme(t)
	for _, rel := range []string{"stormo.yaml", "agents/atlas/../../stormo.yaml", "agents/ghost/agent.yaml",
		"agents/atlas/hermes/config.base.yaml", "/etc/passwd", "../agents/atlas/agent.yaml"} {
		if _, err := Show(inst, rel); code(err) != "unsupported" {
			t.Errorf("%s: %v", rel, err)
		}
	}
}
