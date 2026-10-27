package loop

import (
	"context"
	"fmt"

	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/pkg/learning"
)

// LearningData is owner-only scoped metadata and review candidates, never raw sessions.
type LearningData = learning.RuntimeReviewData
type LearningRequest = learning.RuntimeReviewRequest

func (a *Agent) LearningReview(ctx context.Context, session string, req *LearningRequest) (LearningData, error) {
	if a.Learning == nil {
		return LearningData{}, fmt.Errorf("learning unavailable")
	}
	root, ok := a.Store.Root(session)
	if !ok {
		return LearningData{}, ErrNoSession
	}
	unlock := a.lockLearning(session)
	defer unlock()
	aud := a.learningAudience(session)
	root = runtimelearn.Hash(root)
	var operation *learning.RuntimeReviewOperation
	if req != nil {
		if a.Store.Untrusted(session) {
			return LearningData{}, fmt.Errorf("untrusted conversations cannot produce feedback or proposals")
		}
		check := func(text string) error {
			if a.Env.Redact != nil && a.Env.Redact(text) != text {
				return fmt.Errorf("secret values refused")
			}
			return nil
		}
		if req.ExportID != "" {
			exportAudience := aud
			candidates, lookupErr := a.Learning.Candidates(root, "agent")
			if lookupErr != nil {
				return LearningData{}, lookupErr
			}
			for _, c := range candidates {
				if c.ID == req.ExportID {
					exportAudience = "agent"
				}
			}
			if err := a.Store.WithTrusted(session, func() error { return a.Learning.Export(root, exportAudience, req.ExportID, check) }); err != nil {
				return LearningData{}, err
			}
		} else {
			eventAudience := aud
			events, lookupErr := a.Learning.Events(root, "agent")
			if lookupErr != nil {
				return LearningData{}, lookupErr
			}
			for _, item := range events {
				if item.EventID == req.EventID {
					eventAudience = "agent"
					break
				}
			}
			var e learning.OutcomeEvent
			err := a.Store.WithTrusted(session, func() error {
				var eErr error
				e, eErr = a.Learning.Feedback(root, eventAudience, req.EventID, req.Outcome, req.Origin)
				return eErr
			})
			if err != nil {
				return LearningData{}, err
			}
			if req.Outcome == "corrected" {
				a.queueBackground(session, e, runtimelearn.TriggerFeedbackCorrected, req.Summary)
			}
			if a.Cfg.Reflection {
				if err = a.Learning.Reflect(ctx, a.LLM, a.Env.Memory, session, e, req.Summary, check); err != nil {
					operation = &learning.RuntimeReviewOperation{OK: false, Code: "reflection_failed", Message: "Feedback was saved; reflection did not complete. Check the budget and review the saved evidence.", FeedbackSaved: true}
				}
			}
		}
	}
	events, err := a.Learning.Events(root, aud)
	if err != nil {
		return LearningData{}, err
	}
	if aud != "agent" {
		public, err := a.Learning.Events(root, "agent")
		if err != nil {
			return LearningData{}, err
		}
		events = append(events, public...)
	}
	c, err := a.Learning.Candidates(root, aud)
	if err == nil && aud != "agent" {
		public, e := a.Learning.Candidates(root, "agent")
		if e != nil {
			return LearningData{}, e
		}
		c = append(c, public...)
	}
	if err != nil {
		return LearningData{}, err
	}
	status, err := a.Learning.Status(root, aud)
	if err != nil {
		return LearningData{}, err
	}
	status.ReflectionEnabled = a.Cfg.Reflection
	status.FeedbackAllowed = !a.Store.Untrusted(session)
	status.ExportAllowed = status.FeedbackAllowed
	if req != nil && operation == nil {
		operation = &learning.RuntimeReviewOperation{OK: true, Code: "saved", Message: "Learning review saved.", FeedbackSaved: req.ExportID == ""}
	}
	bs, bc, err := a.Learning.BackgroundSnapshot(root, aud, a.now())
	if err != nil {
		return LearningData{}, err
	}
	if aud != "agent" {
		publicStatus, publicCandidates, err := a.Learning.BackgroundSnapshot(root, "agent", a.now())
		if err != nil {
			return LearningData{}, err
		}
		bs.Pending += publicStatus.Pending
		bs.Completed += publicStatus.Completed
		bs.Failed += publicStatus.Failed
		bs.Interrupted += publicStatus.Interrupted
		bc = append(bc, publicCandidates...)
	}
	background := &learning.BackgroundReviewData{Enabled: a.Cfg.BackgroundLearning, Pending: bs.Pending, Completed: bs.Completed, Failed: bs.Failed, Interrupted: bs.Interrupted, CallsUsed: bs.CallsUsed, TokensReserved: bs.TokensReserved, Candidates: bc}
	return LearningData{Events: events, Candidates: c, Status: status, Operation: operation, Background: background}, nil
}

// Serialize owner feedback/reflection with turns so a root cannot become tainted
// between its authority check and candidate persistence.
func (a *Agent) lockLearning(session string) func() {
	root, _ := a.Store.Root(session)
	sum := runtimelearn.Hash(root)
	index := 0
	for _, b := range []byte(sum) {
		index = (index*31 + int(b)) % len(a.learningLocks)
	}
	mu := &a.learningLocks[index]
	mu.Lock()
	return mu.Unlock
}
