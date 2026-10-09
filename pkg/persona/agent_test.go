package persona

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/config"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

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
	var e *config.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func TestEditAnAgentsLook(t *testing.T) {
	inst := acme(t)
	cur, err := ShowAgent(inst, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if cur.Path != "personas/atlas" || !cur.Editable || cur.Hash == "" || cur.Look.Description == "" {
		t.Fatalf("show %+v", cur)
	}
	l := cur.Look
	l.Sprite = &Sprite{Hair: "#3a2a20", HairStyle: "fade"}
	if _, err := SetAgent(inst, "atlas", l, "stale"); code(err) != "conflict" {
		t.Errorf("stale hash: %v", err)
	}
	got, err := SetAgent(inst, "atlas", l, cur.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Look.Sprite == nil || got.Look.Sprite.HairStyle != "fade" || got.Look.Description != cur.Look.Description || got.Hash == cur.Hash {
		t.Errorf("after set %+v", got)
	}
}

func TestDefineALookForAnAgentWithout(t *testing.T) {
	inst := acme(t)
	// Take atlas's persona away, as an agent made before looks were defined.
	f, err := config.Show(inst, "agents/atlas/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Apply(inst, "agents/atlas/agent.yaml", []byte(`{"persona":null}`), f.Hash); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(inst.Root, "personas", "atlas")); err != nil {
		t.Fatal(err)
	}
	cur, err := ShowAgent(inst, "atlas")
	if err != nil || cur.Path != "" || cur.Hash != "" {
		t.Fatalf("show %+v %v", cur, err)
	}
	got, err := SetAgent(inst, "atlas", Look{Description: "A tall figure.", Desk: &Desk{Props: []string{"books"}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "personas/atlas" || got.Look.Description != "A tall figure." {
		t.Errorf("created %+v", got)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil || a.Persona == nil || a.Persona.Path != "personas/atlas" {
		t.Errorf("agent.yaml persona %+v %v", a, err)
	}
	b, _ := os.ReadFile(filepath.Join(inst.Root, "personas", "atlas", "persona.md"))
	if !strings.Contains(string(b), "# Atlas") {
		t.Errorf("persona.md:\n%s", b)
	}
}

func TestSiblingRepoPersonaIsReadOnly(t *testing.T) {
	inst := acme(t)
	f, err := config.Show(inst, "agents/atlas/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.Apply(inst, "agents/atlas/agent.yaml", []byte(`{"persona":{"repo":"brand-repo"}}`), f.Hash); err != nil {
		t.Fatal(err)
	}
	cur, err := ShowAgent(inst, "atlas")
	if err != nil || cur.Editable {
		t.Fatalf("show %+v %v", cur, err)
	}
	if _, err := SetAgent(inst, "atlas", Look{}, ""); code(err) != "readonly" {
		t.Errorf("set: %v", err)
	}
}
