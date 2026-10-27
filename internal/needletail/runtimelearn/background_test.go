package runtimelearn

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/camfinc/stormo/internal/needletail/llm"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func backgroundFixture(t *testing.T) (*Store, BackgroundTrigger, time.Time) {
	t.Helper()
	s := &Store{Home: t.TempDir(), Trusted: func(_ string, f func() error) error { return f() }}
	e := event()
	e.ExecutionStatus = "failed"
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	return s, BackgroundTrigger{Kind: TriggerToolFailure, Session: "source", Root: e.TaskRoot, Audience: e.Audience, Summary: "Observed tool execution failed; task outcome unknown.", EvidenceRefs: []string{e.EventID}}, time.Date(2026, 10, 27, 10, 0, 0, 0, time.UTC)
}
func TestBackgroundDedupBudgetAndRetry(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	id, err := s.EnqueueBackground(tr, now)
	if err != nil {
		t.Fatal(err)
	}
	tr.Summary = "Reworded delivery"
	again, err := s.EnqueueBackground(tr, now)
	if err != nil || id != again {
		t.Fatal(again, err)
	}
	calls := 0
	reflect := func(context.Context, BackgroundTrigger) (*Candidate, error) {
		calls++
		return nil, &BackgroundRetryableError{Err: fmt.Errorf("known pre-call failure")}
	}
	cfg := BackgroundConfig{DailyCalls: 1, DailyTokens: 512, MaxAttempts: 2}
	if worked, err := s.ProcessBackground(context.Background(), now, cfg, reflect, nil); !worked || err == nil {
		t.Fatal(worked, err)
	}
	if worked, err := s.ProcessBackground(context.Background(), now.Add(time.Minute), cfg, reflect, nil); worked || err == nil {
		t.Fatal(worked, err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
	if worked, err := s.ProcessBackground(context.Background(), now.Add(24*time.Hour), cfg, reflect, nil); !worked || err == nil {
		t.Fatal(worked, err)
	}
	status, _, err := s.BackgroundSnapshot(tr.Root, tr.Audience, now.Add(24*time.Hour))
	if err != nil || status.Failed != 1 || status.CallsUsed != 1 {
		t.Fatal(status, err)
	}
	info, err := os.Stat(filepath.Join(s.Home, BackgroundFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}
func TestBackgroundRequiresEvidenceAndTrust(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	bad := tr
	bad.EvidenceRefs = []string{Hash("invented")}
	if _, err := s.EnqueueBackground(bad, now); err == nil {
		t.Fatal("invented evidence accepted")
	}
	bad = tr
	bad.Kind = TriggerRecovery
	if _, err := s.EnqueueBackground(bad, now); err == nil {
		t.Fatal("failure became success")
	}
	s.Trusted = nil
	if _, err := s.EnqueueBackground(tr, now); err == nil {
		t.Fatal("missing guard accepted")
	}
}
func TestBackgroundQuarantineDuringCallAndCrashRecovery(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	quarantined := false
	s.Trusted = func(_ string, f func() error) error {
		if quarantined {
			return fmt.Errorf("quarantined")
		}
		return f()
	}
	reflect := func(context.Context, BackgroundTrigger) (*Candidate, error) {
		quarantined = true
		return &Candidate{Text: "Review tool failure", Checks: "tool failed", Conditions: "same operation", Sensitivity: "procedural", ReviewDate: "2026-11-27", Action: "new"}, nil
	}
	if worked, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, reflect, nil); !worked || err == nil {
		t.Fatal(worked, err)
	}
	// Reservation survives failure to persist after source became untrusted.
	restarted := &Store{Home: s.Home, Trusted: s.Trusted}
	if err := restarted.RecoverBackground(); err != nil {
		t.Fatal(err)
	}
	status, c, err := restarted.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || status.Interrupted != 1 || len(c) != 0 || status.CallsUsed != 1 {
		t.Fatal(status, c, err)
	}
	if worked, err := restarted.ProcessBackground(context.Background(), now, BackgroundConfig{}, reflect, nil); worked || err != nil {
		t.Fatal(worked, err)
	}
}
func TestBackgroundCandidateScopeAndUnknownVersion(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	reflect := func(context.Context, BackgroundTrigger) (*Candidate, error) {
		return &Candidate{Root: "forged", Audience: "other", EventRefs: []string{"forged"}, Text: "Review failure", Checks: "observed failure", Conditions: "same operation", Sensitivity: "procedural", ReviewDate: "2026-11-27", Action: "new"}, nil
	}
	if worked, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, reflect, nil); !worked || err != nil {
		t.Fatal(worked, err)
	}
	_, c, err := s.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || len(c) != 1 || c[0].Root != tr.Root || c[0].Audience != tr.Audience || c[0].EventRefs[0] != tr.EvidenceRefs[0] {
		t.Fatal(c, err)
	}
	_, other, _ := s.BackgroundSnapshot(tr.Root, "other", now)
	if len(other) != 0 {
		t.Fatal(other)
	}
	if err := s.write(BackgroundFile, map[string]any{"version": 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueBackground(tr, now); err == nil {
		t.Fatal("unknown version overwritten")
	}
}

func TestBackgroundOrdinaryErrorIsTerminal(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	calls := 0
	reflect := func(context.Context, BackgroundTrigger) (*Candidate, error) {
		calls++
		return nil, fmt.Errorf("ambiguous provider timeout")
	}
	if _, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, reflect, nil); err == nil {
		t.Fatal("missing provider error")
	}
	if worked, err := s.ProcessBackground(context.Background(), now.Add(time.Hour), BackgroundConfig{}, reflect, nil); worked || err != nil {
		t.Fatal(worked, err)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestBackgroundPrecallQuarantineDoesNotStarve(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	s.Trusted = func(string, func() error) error { return fmt.Errorf("quarantined") }
	reflect := func(context.Context, BackgroundTrigger) (*Candidate, error) {
		t.Fatal("called model for tainted source")
		return nil, nil
	}
	if worked, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, reflect, nil); !worked || err == nil {
		t.Fatal(worked, err)
	}
	status, _, err := s.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || status.Pending != 0 || status.Interrupted != 1 || status.CallsUsed != 0 {
		t.Fatal(status, err)
	}
}
func TestBackgroundCrashReservationNeverReplayed(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	// Simulate process exit after the durable pre-call reservation.
	s.mu.Lock()
	v, err := s.background()
	if err == nil {
		v.Jobs[0].State = "reserved"
		v.Jobs[0].Attempts = 1
		v.Reservations = []backgroundReservation{{At: now, Tokens: 512}}
		err = s.write(BackgroundFile, v)
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restarted := &Store{Home: s.Home, Trusted: s.Trusted}
	if err := restarted.RecoverBackground(); err != nil {
		t.Fatal(err)
	}
	status, _, err := restarted.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || status.Interrupted != 1 || status.CallsUsed != 1 {
		t.Fatal(status, err)
	}
}

func TestBackgroundCorruptCandidateAndOversizedStateRefused(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	reflect := func(context.Context, BackgroundTrigger) (*Candidate, error) {
		return &Candidate{Text: "Review failure", Checks: "observed failure", Conditions: "same operation", Sensitivity: "procedural", ReviewDate: "2026-11-27", Action: "new"}, nil
	}
	if _, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, reflect, nil); err != nil {
		t.Fatal(err)
	}
	v, err := s.background()
	if err != nil {
		t.Fatal(err)
	}
	v.Jobs[0].Candidate.Audience = "foreign"
	if err := s.write(BackgroundFile, v); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BackgroundSnapshot(tr.Root, tr.Audience, now); err == nil {
		t.Fatal("corrupted candidate scope exposed")
	}
	if err := os.WriteFile(filepath.Join(s.Home, BackgroundFile), make([]byte, backgroundBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.BackgroundSnapshot(tr.Root, tr.Audience, now); err == nil {
		t.Fatal("oversized state accepted")
	}
}

func TestBackgroundReflectorWireContract(t *testing.T) {
	for _, mode := range []string{"null", "malformed", "tool-call"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body["max_tokens"] != float64(512) || body["tools"] != nil {
					t.Errorf("unbounded or tool-enabled request: %+v", body)
				}
				messages := body["messages"].([]any)
				user := messages[len(messages)-1].(map[string]any)["content"].(string)
				for _, private := range []string{"PRIVATE-SESSION", "PRIVATE-ROOT", "PRIVATE-AUDIENCE"} {
					if strings.Contains(user, private) {
						t.Errorf("private source identifier disclosed: %s", private)
					}
				}
				var input map[string]any
				if err := json.Unmarshal([]byte(user), &input); err != nil {
					t.Error(err)
				}
				if len(input) != 3 || input["kind"] != "tool_failure" || input["summary"] != "Observed failure" || len(user) > 8000 {
					t.Errorf("unexpected selected input: %s", user)
				}
				refs, ok := input["evidenceRefs"].([]any)
				if !ok || len(refs) != 1 || refs[0] != Hash("evidence") {
					t.Errorf("wrong selected evidence: %+v", input)
				}
				delta := map[string]any{"content": "null"}
				if mode == "malformed" {
					delta["content"] = "not JSON"
				}
				if mode == "tool-call" {
					delta = map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "forbidden", "type": "function", "function": map[string]any{"name": "terminal", "arguments": "{}"}}}}
				}
				response, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": "stop"}}})
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", response)
			}))
			defer srv.Close()
			reflect := NewBackgroundReflector(&llm.Client{Endpoints: []llm.Endpoint{{BaseURL: srv.URL, Model: "fixture"}}})
			tr := BackgroundTrigger{Kind: TriggerToolFailure, Session: "PRIVATE-SESSION", Root: "PRIVATE-ROOT", Audience: "PRIVATE-AUDIENCE", Summary: "Observed failure", EvidenceRefs: []string{Hash("evidence")}}
			c, err := reflect(context.Background(), tr)
			if mode == "null" {
				if err != nil || c != nil {
					t.Fatal(c, err)
				}
			} else if err == nil {
				t.Fatalf("%s output accepted", mode)
			}
			if requests.Load() != 1 {
				t.Fatalf("requests=%d", requests.Load())
			}
			tr.Summary = strings.Repeat("x", 8001)
			if _, err := reflect(context.Background(), tr); err == nil {
				t.Fatal("oversized input accepted")
			}
			if requests.Load() != 1 {
				t.Fatal("oversized input reached provider")
			}
		})
	}
}
func TestBackgroundConcurrentProcessingCallsProviderOnce(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	start := make(chan struct{})
	entered := make(chan struct{})
	release := make(chan struct{})
	results := make(chan error, workers)
	var calls atomic.Int32
	reflect := func(context.Context, BackgroundTrigger) (*Candidate, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		<-release
		return nil, nil
	}
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			_, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, reflect, nil)
			results <- err
		}()
	}
	close(start)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider call did not start")
	}
	close(release)
	for i := 0; i < workers; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("provider called %d times for one job", calls.Load())
	}
	status, _, err := s.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || status.Completed != 1 || status.CallsUsed != 1 {
		t.Fatal(status, err)
	}
}

func TestBackgroundCandidateTooLargeAfterHostStampIsTerminal(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	// Multiple genuine evidence references make final provenance significantly
	// larger than the model candidate, while remaining within trigger bounds.
	for i := 1; i < 8; i++ {
		e := event()
		e.ExecutionStatus = "failed"
		e.EventID = Hash(fmt.Sprintf("extra-%d", i))
		e.Origin = e.EventID
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
		tr.EvidenceRefs = append(tr.EvidenceRefs, e.EventID)
	}
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	c := &Candidate{Text: "Review failure", Checks: strings.Repeat("x", 3200), Conditions: "same operation", Sensitivity: "procedural", ReviewDate: "2026-11-27", Action: "new"}
	if err := validateBackgroundCandidate(c); err != nil {
		t.Fatal("fixture should fit before provenance", err)
	}
	worked, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, func(context.Context, BackgroundTrigger) (*Candidate, error) { return c, nil }, nil)
	if !worked || err == nil {
		t.Fatal("oversized stamped candidate accepted", worked, err)
	}
	status, candidates, err := s.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || status.Failed != 1 || len(candidates) != 0 || status.CallsUsed != 1 {
		t.Fatal(status, candidates, err)
	}
	// State is still readable after restart; no invalid candidate poisoned it.
	restarted := &Store{Home: s.Home, Trusted: s.Trusted}
	if err := restarted.RecoverBackground(); err != nil {
		t.Fatal(err)
	}
}
