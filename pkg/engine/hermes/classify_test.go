package hermes

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
)

// testdata/classify.json pins how every snapshot rule treats ~50 representative runtime paths. A
// misclassified path silently leaks conversation PII toward git or drops tool state.
func TestClassify(t *testing.T) {
	body, err := os.ReadFile("../../../testdata/classify.json")
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		StateDir string `json:"stateDir"`
		Cases    []struct {
			Path   string  `json:"path"`
			Class  *string `json:"class"`
			Glob   *string `json:"glob"`
			SQLite bool    `json:"sqlite"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load("../../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Names.StateDir != want.StateDir {
		t.Fatalf("fixture state dir %q, cases written for %q", inst.Names.StateDir, want.StateDir)
	}
	rules := New(inst).Layout().Rules
	for _, c := range want.Cases {
		got := engine.Classify(c.Path, rules)
		switch {
		case c.Class == nil && got != nil:
			t.Errorf("%s: want unclaimed, got %s (%s)", c.Path, got.Class, got.Glob)
		case c.Class != nil && got == nil:
			t.Errorf("%s: want %s (%s), got unclaimed", c.Path, *c.Class, *c.Glob)
		case c.Class != nil && (string(got.Class) != *c.Class || got.Glob != *c.Glob || got.SQLite != c.SQLite):
			t.Errorf("%s: want %s via %s, got %s via %s", c.Path, *c.Class, *c.Glob, got.Class, got.Glob)
		}
	}
}
