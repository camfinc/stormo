package loop

import (
	"testing"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
)

// learningEngine is a fake engine with the optional learning interfaces an engine may implement
// (engine.OutcomeEvidence, engine.LearningProposals) over the agent's real engine, so the
// engine-neutral learning code is tested without naming a real engine that has them.
type learningEngine struct{ engine.Engine }

func (learningEngine) OutcomePath() string           { return "state/fake/outcomes.json" }
func (learningEngine) LearningProposalsPath() string { return "state/fake/proposals.json" }

// withLearningEngine makes every agent's engine a learningEngine for the test.
func withLearningEngine(t *testing.T) learningEngine {
	t.Helper()
	get := engineFor
	engineFor = func(kind string, inst *instance.Instance) (engine.Engine, error) {
		eng, err := get(kind, inst)
		if err != nil {
			return nil, err
		}
		return learningEngine{eng}, nil
	}
	t.Cleanup(func() { engineFor = get })
	return learningEngine{}
}
