package needletail

import (
	"testing"

	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
)

// Needletail implements the optional learning interfaces the engine-neutral code tests on a fake
// engine: the paths it names are the runtime's own files, the two the dream reads are Learning
// (the only class a dream reads), and the private ones never are.
func TestLearningInterfaces(t *testing.T) {
	inst, err := instance.Load(minimal)
	if err != nil {
		t.Fatal(err)
	}
	n := New(inst)
	var eng engine.Engine = n
	outcomes, ok1 := eng.(engine.OutcomeEvidence)
	proposals, ok2 := eng.(engine.LearningProposals)
	private, ok3 := eng.(engine.PrivateLearningState)
	_, ok4 := eng.(engine.PrivateLearningTrees)
	rt, ok5 := eng.Runtime().(engine.LearningRuntime)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		t.Fatalf("interfaces: %v %v %v %v %v", ok1, ok2, ok3, ok4, ok5)
	}
	if outcomes.OutcomePath() != runtimelearn.OutcomesExportFile || proposals.LearningProposalsPath() != runtimelearn.ProposalsFile {
		t.Errorf("paths %q %q differ from the runtime's", outcomes.OutcomePath(), proposals.LearningProposalsPath())
	}
	rules := n.Layout().Rules
	for _, p := range []string{outcomes.OutcomePath(), proposals.LearningProposalsPath()} {
		if r := engine.Classify(p, rules); r == nil || r.Class != engine.Learning {
			t.Errorf("%s is not Learning: %+v", p, r)
		}
	}
	for _, p := range private.PrivateLearningPaths() {
		if p == outcomes.OutcomePath() || p == proposals.LearningProposalsPath() {
			continue
		}
		if r := engine.Classify(p, rules); r == nil || r.Class == engine.Learning {
			t.Errorf("private %s is classified %+v", p, r)
		}
	}
	if rt.LearningKeyName() != "NEEDLETAIL_LEARNING_KEY" || rt.LearningPath("a b") != "/v1/sessions/a%20b/learning" {
		t.Errorf("learning runtime %q %q", rt.LearningKeyName(), rt.LearningPath("a b"))
	}
}
