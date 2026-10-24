package needletail

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/shared"
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
		"state/needletail/monitor/j.out": engine.Raw, "state/needletail/jobs/j/t.md": engine.Raw,
		// the memory archive holds client data; its counters are ids and numbers only
		"memories/RECORDS.md": engine.Raw, "state/needletail/memory-usage.json": engine.State, "state/needletail/memory.lock": engine.State,
		// the pending follow-ups survive a restart; what they say is client data, never the dream's
		"state/needletail/followups.json": engine.Raw, "state/needletail/followups.lock": engine.State,
		// Luau's key-value store: a handler can keep message text there
		"state/needletail/kv.json": engine.Raw} {
		if r := engine.Classify(p, rules); r == nil || r.Class != class {
			t.Errorf("%s: want %s, got %v", p, class, r)
		}
	}
	l := New(inst).Layout()
	if l.Home != "/opt/data" || l.Schedules != "cron/jobs.json" || l.ScheduleSource != "" || l.SkillUsage != "skills/.usage.json" || l.RuntimeUID != 10000 ||
		l.ScriptsDir != "scripts" {
		t.Errorf("layout = %+v", l)
	}
}

// The runtime's uid (the binary's, wire.RuntimeUID) is the Layout's, which rehydrate hands the
// home to and the EFS access points use; the agent's commands run as another (wire.ExecUID).
func TestRuntimeUsers(t *testing.T) {
	l := New(mustInstance(t)).Layout()
	if l.RuntimeUID != wire.RuntimeUID || l.RuntimeGID != wire.RuntimeGID || wire.RuntimeUID != shared.PosixUID || wire.ExecUID == wire.RuntimeUID {
		t.Fatalf("layout %d:%d, wire %d:%d exec %d, shared %d", l.RuntimeUID, l.RuntimeGID, wire.RuntimeUID, wire.RuntimeGID, wire.ExecUID, shared.PosixUID)
	}
}

func mustInstance(t *testing.T) *instance.Instance {
	t.Helper()
	inst, err := instance.Load(minimal)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}
