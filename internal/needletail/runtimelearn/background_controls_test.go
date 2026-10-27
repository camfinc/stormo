package runtimelearn

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBackgroundCancellationSuppressesLateResultAndCallback(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	id, err := s.EnqueueBackground(tr, now)
	if err != nil {
		t.Fatal(err)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	finished := false
	go func() {
		_, err := s.ProcessBackgroundWithResult(context.Background(), now, BackgroundConfig{}, func(context.Context, BackgroundTrigger) (*Candidate, error) {
			close(started)
			<-release
			return &Candidate{Text: "Review failure", Checks: "Observed failure", Conditions: "Same operation", Sensitivity: "procedural", ReviewDate: "2026-11-27", Action: "new"}, nil
		}, nil, func(BackgroundTrigger, *Candidate) error { finished = true; return nil })
		done <- err
	}()
	<-started
	if err := s.CancelBackground(tr.Root, "wrong", id); err == nil {
		t.Fatal("cross-audience cancellation")
	}
	if err := s.CancelBackground(tr.Root, tr.Audience, id); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	jobs, err := s.BackgroundJobs(tr.Root, tr.Audience, now)
	if err != nil || len(jobs) != 1 || jobs[0].State != "cancelled" || finished {
		t.Fatal(jobs, finished, err)
	}
	status, candidates, err := s.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || status.CallsUsed != 1 || len(candidates) != 0 {
		t.Fatal(status, candidates, err)
	}
	if err := s.CancelBackground(tr.Root, tr.Audience, id); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundExplicitUncertainRetryBoundedAndBudgeted(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	id, err := s.EnqueueBackground(tr, now)
	if err != nil {
		t.Fatal(err)
	}
	fail := func(context.Context, BackgroundTrigger) (*Candidate, error) {
		return nil, fmt.Errorf("uncertain provider execution")
	}
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if err := s.RetryBackground(tr.Root, tr.Audience, id, now); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, fail, nil); err == nil {
			t.Fatal("missing model error")
		}
	}
	if err := s.RetryBackground(tr.Root, tr.Audience, id, now); err == nil {
		t.Fatal("unbounded owner retry")
	}
	restarted := &Store{Home: s.Home, Trusted: s.Trusted}
	jobs, err := restarted.BackgroundJobs(tr.Root, tr.Audience, now)
	if err != nil || jobs[0].Attempts != 3 || jobs[0].OwnerRetries != 2 {
		t.Fatal(jobs, err)
	}
	status, _, err := restarted.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || status.CallsUsed != 3 {
		t.Fatal(status, err)
	}
	if err := restarted.RetryBackground(tr.Root, "wrong", id, now); err == nil {
		t.Fatal("cross-scope retry")
	}
}

func TestBackgroundPruneKeepsDedupeAndBudget(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	id, err := s.EnqueueBackground(tr, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, func(context.Context, BackgroundTrigger) (*Candidate, error) { return nil, nil }, nil); err != nil {
		t.Fatal(err)
	}
	// A recently reserved call must survive compaction of older terminal details.
	s.mu.Lock()
	v, err := s.background()
	if err == nil {
		v.Jobs[0].Updated = now.Add(-31 * 24 * time.Hour)
		err = s.writeBackground(v)
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneBackground(tr.Root, "wrong", now); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if n, err := s.PruneBackground(tr.Root, tr.Audience, now); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	restarted := &Store{Home: s.Home, Trusted: s.Trusted}
	if got, err := restarted.EnqueueBackground(tr, now); err != nil || got != id {
		t.Fatal(got, err)
	}
	jobs, err := restarted.BackgroundJobs(tr.Root, tr.Audience, now)
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	status, _, err := restarted.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || status.CallsUsed != 1 {
		t.Fatal(status, err)
	}
	if worked, err := restarted.ProcessBackground(context.Background(), now, BackgroundConfig{}, func(context.Context, BackgroundTrigger) (*Candidate, error) {
		t.Fatal("replayed tombstone")
		return nil, nil
	}, nil); worked || err != nil {
		t.Fatal(worked, err)
	}
}

func TestBackgroundV1MigrationPreservesReservationAndBackup(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	id := backgroundIdentity(tr)
	legacy := backgroundState{Version: 1, Jobs: []backgroundJob{{ID: id, Trigger: tr, State: "reserved", Attempts: 1, Next: now}}, Reservations: []backgroundReservation{{At: now, Tokens: 512}}}
	if err := s.write(BackgroundFile, legacy); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(s.Home, BackgroundFile))
	if err != nil {
		t.Fatal(err)
	}
	var pretty []byte
	var indented any
	if err := json.Unmarshal(raw, &indented); err != nil {
		t.Fatal(err)
	}
	pretty, err = json.MarshalIndent(indented, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Home, BackgroundFile), pretty, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(s.Home, BackgroundFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverBackground(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(filepath.Join(s.Home, BackgroundBackupFile))
	if err != nil || string(before) != string(backup) {
		t.Fatal("migration backup changed", err)
	}
	migrated, err := os.ReadFile(filepath.Join(s.Home, BackgroundFile))
	if err != nil {
		t.Fatal(err)
	}
	var v backgroundState
	if err := json.Unmarshal(migrated, &v); err != nil || v.Version != 2 || v.Jobs[0].State != "interrupted" || len(v.Reservations) != 1 {
		t.Fatal(v, err)
	}
	if err := s.RecoverBackground(); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(s.Home, BackgroundBackupFile))
	if string(again) != string(before) {
		t.Fatal("backup replaced on repeated migration")
	}
}

func TestBackgroundRetryRechecksTrustAndConsumesFreshBudget(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	id, err := s.EnqueueBackground(tr, now)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	reflect := func(context.Context, BackgroundTrigger) (*Candidate, error) {
		calls++
		return nil, fmt.Errorf("ambiguous call")
	}
	cfg := BackgroundConfig{DailyCalls: 1, DailyTokens: 512}
	if _, err := s.ProcessBackground(context.Background(), now, cfg, reflect, nil); err == nil {
		t.Fatal("missing model error")
	}
	trusted := s.Trusted
	s.Trusted = func(string, func() error) error { return fmt.Errorf("quarantined") }
	if err := s.RetryBackground(tr.Root, tr.Audience, id, now); err == nil {
		t.Fatal("quarantined owner retry admitted")
	}
	s.Trusted = trusted
	if err := s.RetryBackground(tr.Root, tr.Audience, id, now); err != nil {
		t.Fatal(err)
	}
	if worked, err := s.ProcessBackground(context.Background(), now, cfg, reflect, nil); worked || err != ErrBackgroundBudget {
		t.Fatal(worked, err)
	}
	if calls != 1 {
		t.Fatal("budget reset", calls)
	}
	if worked, err := s.ProcessBackground(context.Background(), now.Add(24*time.Hour), cfg, reflect, nil); !worked || err == nil {
		t.Fatal(worked, err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestBackgroundFinishSuppressedByLateQuarantine(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	quarantine := false
	s.Trusted = func(_ string, f func() error) error {
		if quarantine {
			return fmt.Errorf("quarantined")
		}
		return f()
	}
	finished := false
	_, err := s.ProcessBackgroundWithResult(context.Background(), now, BackgroundConfig{}, func(context.Context, BackgroundTrigger) (*Candidate, error) {
		quarantine = true
		return &Candidate{Text: "Review failure", Checks: "Observed failure", Conditions: "Same operation", Sensitivity: "procedural", ReviewDate: "2026-11-27", Action: "new"}, nil
	}, nil, func(BackgroundTrigger, *Candidate) error { finished = true; return nil })
	if err == nil || finished {
		t.Fatal("late quarantine emitted proposal", err)
	}
}

func TestBackgroundPruneRetainsScopedUnreviewedCandidate(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	id, err := s.EnqueueBackground(tr, now)
	if err != nil {
		t.Fatal(err)
	}
	candidate := &Candidate{Text: "Review tool failure", Checks: "Observed failure; cause unknown", Conditions: "Before retrying operation", Sensitivity: "procedural", ReviewDate: "2026-11-27", Action: "new"}
	if _, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, func(context.Context, BackgroundTrigger) (*Candidate, error) { return candidate, nil }, nil); err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneBackground(tr.Root, tr.Audience, now.Add(31*24*time.Hour)); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	restarted := &Store{Home: s.Home, Trusted: s.Trusted}
	_, got, err := restarted.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || len(got) != 1 || got[0].ID != candidate.ID || got[0].Text != candidate.Text || got[0].Checks != candidate.Checks || got[0].EventRefs[0] != tr.EvidenceRefs[0] {
		t.Fatal(got, err)
	}
	for _, scope := range [][2]string{{Hash("foreign-root"), tr.Audience}, {tr.Root, "foreign-audience"}} {
		_, foreign, err := restarted.BackgroundSnapshot(scope[0], scope[1], now)
		if err != nil || len(foreign) != 0 {
			t.Fatal("retained candidate leaked", foreign, err)
		}
	}
	jobs, err := restarted.BackgroundJobs(tr.Root, tr.Audience, now)
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
	if gotID, err := restarted.EnqueueBackground(tr, now); err != nil || gotID != id {
		t.Fatal(gotID, err)
	}
	if worked, err := restarted.ProcessBackground(context.Background(), now, BackgroundConfig{}, func(context.Context, BackgroundTrigger) (*Candidate, error) {
		t.Fatal("compacted proposal replayed")
		return nil, nil
	}, nil); worked || err != nil {
		t.Fatal(worked, err)
	}
	raw, err := os.ReadFile(filepath.Join(s.Home, BackgroundFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), tr.Summary) || strings.Contains(string(raw), `"session"`) {
		t.Fatal("compaction retained source scheduling details")
	}
}

func TestBackgroundPruneCapacityFailurePreservesPendingReview(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProcessBackground(context.Background(), now, BackgroundConfig{}, func(context.Context, BackgroundTrigger) (*Candidate, error) {
		return &Candidate{Text: "Review tool failure", Checks: "Observed failure", Conditions: "Same operation", Sensitivity: "procedural", ReviewDate: "2026-11-27", Action: "new"}, nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	v, err := s.background()
	if err == nil {
		for i := 0; i < backgroundTombstoneCapacity; i++ {
			v.Tombstones = append(v.Tombstones, backgroundTombstone{ID: Hash(fmt.Sprintf("old-terminal-%d", i)), Root: tr.Root, Audience: tr.Audience, State: "complete"})
		}
		err = s.writeBackground(v)
	}
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(s.Home, BackgroundFile))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.PruneBackground(tr.Root, tr.Audience, now.Add(31*24*time.Hour)); err == nil || n != 0 {
		t.Fatal("full tombstone store compacted", n, err)
	}
	after, err := os.ReadFile(filepath.Join(s.Home, BackgroundFile))
	if err != nil || string(after) != string(before) {
		t.Fatal("failed compaction modified state", err)
	}
	_, candidates, err := s.BackgroundSnapshot(tr.Root, tr.Audience, now)
	if err != nil || len(candidates) != 1 {
		t.Fatal("capacity failure erased review", candidates, err)
	}
}

func TestBackgroundRetainedCandidateByteLimitPreservesStore(t *testing.T) {
	s, tr, now := backgroundFixture(t)
	if _, err := s.EnqueueBackground(tr, now); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(s.Home, BackgroundFile))
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	v, err := s.background()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < backgroundTombstoneCapacity; i++ {
		refs := []string{Hash(fmt.Sprintf("retained-evidence-%d", i))}
		trigger := BackgroundTrigger{Kind: TriggerToolFailure, Root: tr.Root, Audience: tr.Audience, EvidenceRefs: refs}
		id := backgroundIdentity(trigger)
		c := &Candidate{ID: Hash(id + "\n" + strings.Repeat("T", 1000)), Text: strings.Repeat("T", 1000), Checks: strings.Repeat("C", 1000), Conditions: strings.Repeat("D", 1000), Sensitivity: "procedural", ReviewDate: "2026-11-27", Action: "new", Root: tr.Root, Audience: tr.Audience, EventRefs: refs}
		encoded, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		c.Exceptions = strings.Repeat("E", 3990-len(encoded))
		if err := validateBackgroundCandidate(c); err != nil {
			t.Fatal(err)
		}
		v.Tombstones = append(v.Tombstones, backgroundTombstone{ID: id, Root: tr.Root, Audience: tr.Audience, Kind: trigger.Kind, EvidenceRefs: refs, State: "complete", Attempts: 1, Candidate: c})
	}
	encoded, err := json.Marshal(v)
	if err != nil || len(encoded) <= backgroundBytes {
		t.Fatal("fixture did not fill byte capacity", len(encoded), err)
	}
	if err := s.writeBackground(v); err == nil {
		t.Fatal("encoded byte capacity ignored")
	}
	after, err := os.ReadFile(filepath.Join(s.Home, BackgroundFile))
	if err != nil || string(after) != string(before) {
		t.Fatal("byte limit failure rewrote live queue", err)
	}
}
