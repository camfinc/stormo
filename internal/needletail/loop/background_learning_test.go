package loop

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/learning"
)

func backgroundReviewAgent(t *testing.T) (*Agent, learning.OutcomeEvent) {
	t.Helper()
	srv := httptest.NewServer(&stub{pick: func(map[string]any) string { t.Error("unexpected LLM call"); return say("null") }})
	t.Cleanup(srv.Close)
	a := newAgent(t, srv)
	a.Cfg.BackgroundLearning = true
	a.Learning = &runtimelearn.Store{Home: a.Cfg.Home, Trusted: a.Store.WithTrusted}
	if _, err := a.Store.Create(wire.Session{ID: "source", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	e := learning.OutcomeEvent{EventID: runtimelearn.Hash("observed"), TaskRoot: runtimelearn.Hash("source"), SubjectKind: "skill", SubjectID: runtimelearn.Hash("procedure"), SubjectRevision: runtimelearn.Hash("revision"), At: a.now().Format("2006-01-02T15:04:05Z"), Audience: a.learningAudience("source"), SourceType: "runtime", Origin: runtimelearn.Hash("call"), ExecutionStatus: "failed", TaskOutcome: "unknown"}
	if err := a.Learning.Append(e); err != nil {
		t.Fatal(err)
	}
	return a, e
}
func TestBackgroundQueueDisabledQuarantinedAndForeignRoot(t *testing.T) {
	for _, scenario := range []string{"disabled", "quarantined", "foreign-root"} {
		t.Run(scenario, func(t *testing.T) {
			a, e := backgroundReviewAgent(t)
			switch scenario {
			case "disabled":
				a.Cfg.BackgroundLearning = false
			case "quarantined":
				if err := a.Store.MarkUntrusted("source"); err != nil {
					t.Fatal(err)
				}
			case "foreign-root":
				e.TaskRoot = runtimelearn.Hash("other")
			}
			a.queueBackground("source", e, runtimelearn.TriggerToolFailure, "Observed failed execution")
			status, _, err := a.Learning.BackgroundSnapshot(runtimelearn.Hash("source"), e.Audience, a.now())
			if err != nil || status.Pending != 0 {
				t.Fatal(status, err)
			}
		})
	}
}
func TestBackgroundCorrectionQueuesAndRefusesSecretSummary(t *testing.T) {
	for _, secret := range []bool{false, true} {
		t.Run(map[bool]string{false: "safe", true: "secret"}[secret], func(t *testing.T) {
			a, e := backgroundReviewAgent(t)
			summary := "Owner verified a correction"
			if secret {
				summary = "fixture-secret"
			}
			a.Env.Redact = func(s string) string { return strings.ReplaceAll(s, "fixture-secret", "[redacted]") }
			out, err := a.LearningReview(context.Background(), "source", &LearningRequest{EventID: e.EventID, Outcome: "corrected", Origin: "owner-check", Summary: summary})
			if err != nil {
				t.Fatal(err)
			}
			expected := 1
			if secret {
				expected = 0
			}
			if out.Background == nil || out.Background.Pending != expected || len(out.Events) != 2 {
				t.Fatalf("background=%+v events=%d", out.Background, len(out.Events))
			}
			if out.Status.CallsUsed != 0 || out.Status.CallsLimit != 100 {
				t.Fatal("background altered explicit reflection budget", out.Status)
			}
		})
	}
}
func TestBackgroundLearningReviewIsolatesPrivateRoots(t *testing.T) {
	a, e := backgroundReviewAgent(t)
	a.queueBackground("source", e, runtimelearn.TriggerToolFailure, "Observed failed execution")
	if _, err := a.Store.Create(wire.Session{ID: "other", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	other, err := a.LearningReview(context.Background(), "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.Background == nil || other.Background.Pending != 0 || len(other.Background.Candidates) != 0 || len(other.Events) != 0 {
		t.Fatalf("foreign review leaked %+v", other)
	}
	own, err := a.LearningReview(context.Background(), "source", nil)
	if err != nil || own.Background.Pending != 1 {
		t.Fatal(own.Background, err)
	}
}
func TestFailedSkillExecutionAutomaticallyQueuesBackground(t *testing.T) {
	st := &stub{pick: func(req map[string]any) string {
		if lastRow(req)["role"] == "user" {
			return callTool("failure", "terminal", `{"command":"exit 7","skill":"crm-api"}`)
		}
		return say("Execution failed; outcome unknown.")
	}}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	a.Cfg.BackgroundLearning = true
	a.Env.Skills = &skills.Library{Home: a.Cfg.Home}
	a.Learning = &runtimelearn.Store{Home: a.Cfg.Home, Trusted: a.Store.WithTrusted}
	if _, err := a.Store.Create(wire.Session{ID: "execution", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), "execution", "Check the procedure", Options{}); err != nil {
		t.Fatal(err)
	}
	root, aud := runtimelearn.Hash("execution"), a.learningAudience("execution")
	events, err := a.Learning.Events(root, aud)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ExecutionStatus != "failed" || events[0].TaskOutcome != "unknown" {
		t.Fatalf("events=%+v", events)
	}
	status, _, err := a.Learning.BackgroundSnapshot(root, aud, a.now())
	if err != nil || status.Pending != 1 || status.CallsUsed != 0 {
		t.Fatal(status, err)
	}
}
