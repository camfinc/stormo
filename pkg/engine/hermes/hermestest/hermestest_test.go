package hermestest_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/engine/hermes/hermestest"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// Every agent of the example is laid over, so the copy is all Hermes, and each one loads as a
// Hermes agent with a valid version and its engine files under engine/hermes only.
func TestEveryExampleAgentIsOnHermes(t *testing.T) {
	root := hermestest.Instance(t)
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	ids := manifest.AgentIDs(root)
	example, err := os.ReadDir(filepath.Join("..", "..", "..", "..", "examples", "minimal", "agents"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{}
	for _, d := range example {
		if d.IsDir() {
			want = append(want, d.Name())
		}
	}
	if !slices.Equal(ids, want) || len(ids) == 0 {
		t.Fatalf("agents %v, the example's %v", ids, want)
	}
	h := hermes.New(inst)
	for _, id := range ids {
		a, err := manifest.Load(root, id, inst.Names.Secret)
		if err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		if a.Engine.Kind != h.Kind() {
			t.Errorf("%s: engine %s", id, a.Engine.Kind)
		}
		if err := engine.CheckVersion(h, a); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		engines, _ := os.ReadDir(filepath.Join(root, "agents", id, "engine"))
		for _, e := range engines {
			if e.Name() != h.Kind() {
				t.Errorf("%s: engine/%s", id, e.Name())
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "agents", "atlas", filepath.FromSlash(hermes.ConfigFile))); err != nil {
		t.Error(err)
	}
}
