package loop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/pkg/learning"
)

// queueBackground accepts only host observations already stored as evidence. No
// transcript, command arguments, tool output, destination or provider error is copied.
func (a *Agent) queueBackground(session string, e learning.OutcomeEvent, kind runtimelearn.TriggerKind, summary string) {
	if a.Cfg == nil || !a.Cfg.BackgroundLearning || a.Learning == nil {
		return
	}
	root, ok := a.Store.Root(session)
	if !ok || runtimelearn.Hash(root) != e.TaskRoot {
		return
	}
	if err := tools.SecretCheck(a.Env.Redact)(summary); err != nil {
		return
	}
	_, err := a.Learning.EnqueueBackground(runtimelearn.BackgroundTrigger{Kind: kind, Session: session, Root: e.TaskRoot, Audience: e.Audience, Summary: summary, EvidenceRefs: []string{e.EventID}}, a.now())
	if err != nil {
		a.logf("background learning: observation not queued")
	}
}

// RunBackgroundLearning is a single serve-owned worker. It yields admission to
// foreground turns, processes at most one bounded job per tick and stops on shutdown.
// A oneshot process does not start an independent worker for the same home.
func (a *Agent) RunBackgroundLearning(ctx context.Context) {
	if a.Cfg == nil || !a.Cfg.BackgroundLearning || a.Learning == nil {
		return
	}
	if err := a.Learning.RecoverBackground(); err != nil {
		a.logf("background learning: recovery refused; worker disabled")
		return
	}
	guarded := func(ctx context.Context, t runtimelearn.BackgroundTrigger) (*runtimelearn.Candidate, error) {
		root, ok := a.Store.Root(t.Session)
		if !ok || runtimelearn.Hash(root) != t.Root || a.Store.Untrusted(t.Session) {
			return nil, fmt.Errorf("source conversation unavailable")
		}
		return a.reflectWithSkill(ctx, t)
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.activeTurns.Load() != 0 {
				continue
			}
			if _, err := a.Learning.ProcessBackgroundWithResult(ctx, a.now(), runtimelearn.BackgroundConfig{}, guarded, tools.SecretCheck(a.Env.Redact), a.stageBackgroundSkill); err != nil && !errors.Is(err, runtimelearn.ErrBackgroundBudget) && ctx.Err() == nil {
				a.logf("background learning: job deferred or refused; inspect owner learning status")
			}
		}
	}
}
