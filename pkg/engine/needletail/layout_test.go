package needletail

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
)

// testdata/classify.json pins every snapshot rule (docs/needletail.md §6, §8): a misclassified path
// leaks conversation PII toward git or drops tool state.
func TestClassify(t *testing.T) {
	body, err := os.ReadFile("testdata/classify.json")
	if err != nil {
		t.Fatal(err)
	}
	var want struct {
		StateDir string `json:"stateDir"`
		Cases    []struct {
			Path  string  `json:"path"`
			Class *string `json:"class"`
			Glob  *string `json:"glob"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(minimal)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Names.StateDir != want.StateDir {
		t.Fatalf("fixture state dir %q, cases written for %q", inst.Names.StateDir, want.StateDir)
	}
	rules := New(inst).Layout().Rules
	for _, r := range rules {
		if r.SQLite {
			t.Errorf("%s: needletail keeps no SQLite", r.Glob)
		}
	}
	for _, c := range want.Cases {
		got := engine.Classify(c.Path, rules)
		switch {
		case c.Class == nil && got != nil && !isRebuilt(c.Path):
			t.Errorf("%s: want unclaimed, got %s (%s)", c.Path, got.Class, got.Glob)
		case c.Class == nil && got != nil:
			t.Errorf("%s: rebuilt from the baseline, but claimed by %s", c.Path, got.Glob)
		case c.Class != nil && got == nil:
			t.Errorf("%s: want %s (%s), got unclaimed", c.Path, *c.Class, *c.Glob)
		case c.Class != nil && (string(got.Class) != *c.Class || got.Glob != *c.Glob):
			t.Errorf("%s: want %s via %s, got %s via %s", c.Path, *c.Class, *c.Glob, got.Class, got.Glob)
		}
	}
	// The runtime's own paths, by class.
	for p, class := range map[string]engine.FileClass{"sessions/x.jsonl": engine.Raw, "logs/audit/s.jsonl": engine.Raw,
		"skills/a/b/tools/t.luau": engine.Learning, "skills/a/b/handlers/h.luau": engine.Learning, "state/needletail/idem.json": engine.State,
		// a monitor's output and a run's reply are client data
		"state/needletail/monitor/j.out": engine.Raw, "state/needletail/jobs/j/t.md": engine.Raw} {
		if r := engine.Classify(p, rules); r == nil || r.Class != class {
			t.Errorf("%s: want %s, got %v", p, class, r)
		}
	}
	l := New(inst).Layout()
	if l.Home != "/opt/data" || l.Schedules != "cron/jobs.json" || l.ScheduleSource != "" || l.SkillUsage != "skills/.usage.json" || l.RuntimeUID != 10000 {
		t.Errorf("layout = %+v", l)
	}
}
