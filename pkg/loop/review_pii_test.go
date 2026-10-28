package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/engine/hermes/hermestest"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

// hermesLoop is an instance with the Hermes atlas agent, its engine and a nap store: the learning
// loop as a live Hermes agent runs it.
func hermesLoop(t *testing.T) (root, home string, inst *instance.Instance, nap func(at time.Time)) {
	t.Helper()
	tmp := t.TempDir()
	root = filepath.Join(tmp, "instance")
	example := hermestest.Instance(t)
	copyDir(t, filepath.Join(example, "units"), filepath.Join(root, "units"))
	copyDir(t, filepath.Join(example, "agents", "atlas"), filepath.Join(root, "agents", "atlas"))
	write(t, filepath.Join(root, "stormo.yaml"), read(t, filepath.Join(example, "stormo.yaml")))
	os.RemoveAll(filepath.Join(root, "agents", "atlas", "learnings"))
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	eng := hermes.New(inst)
	store := FsStore{Root: filepath.Join(tmp, "store")}
	home = filepath.Join(tmp, "home")
	agent, err := manifest.Load(root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	r, err := build.Agent(inst, "atlas", build.Options{Out: filepath.Join(tmp, "baseline")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Rehydrate(RehydrateOptions{Agent: agent, Engine: eng, Home: home, BaselineDir: r.Out, Store: store, Scrubber: learning.NewScrubber(inst)}); err != nil {
		t.Fatal(err)
	}
	nap = func(at time.Time) {
		t.Helper()
		m, err := NapOnce(NapOptions{Agent: "atlas", Unit: "sales", Engine: eng, Home: home, Store: store, Instance: "task-a", BaselineDir: r.Out, Now: at})
		if err != nil || m == nil {
			t.Fatalf("nap: %v %v", m, err)
		}
		if _, err := Dream(inst, "atlas", store); err != nil {
			t.Fatal(err)
		}
	}
	return root, home, inst, nap
}

// Item 18 (Claude M7): a reviewer's rewrite that removes a lesson's personal data clears its
// flags for good: the next dream's sighting of the old text (the same ledger id) does not flag it
// again, while a sighting of the text as it stands still unions its flags (Hermes path).
func TestReviewerPIICleanupSurvivesTheNextDream(t *testing.T) {
	root, home, _, nap := hermesLoop(t)
	write(t, filepath.Join(home, "memories/MEMORY.md"), "Client John Doe phone +1 415 555 0123 wants morning calls.")
	at := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	nap(at)
	ledger, _ := learning.LoadLedger(root, "atlas")
	var flagged *learning.Learning
	for _, e := range ledger {
		if strings.Contains(e.Text, "[PHONE]") {
			flagged = e
		}
	}
	if flagged == nil || len(flagged.PII) == 0 {
		t.Fatalf("no flagged entry: %+v", ledger)
	}
	clean := "A client wants morning calls."
	if _, err := Decide(root, "atlas", []string{flagged.ID}, Change{Text: &clean}); err != nil {
		t.Fatal(err)
	}
	// the agent still holds the old text: the next nap sights it again
	write(t, filepath.Join(home, "memories/MEMORY.md"), strings.Join([]string{"Client John Doe phone +1 415 555 0123 wants morning calls.", "Dana prefers deals grouped by close date."}, "\n§\n"))
	nap(at.Add(time.Hour))
	ledger, _ = learning.LoadLedger(root, "atlas")
	for _, e := range ledger {
		if e.ID == flagged.ID && (e.Text != clean || len(e.PII) != 0) {
			t.Fatalf("the reviewer's cleanup was undone: %+v", e)
		}
	}
}
