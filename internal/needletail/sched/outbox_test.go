package sched

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

type outboxDeliverer struct {
	calls     []Delivery
	fail      map[string]error
	alerts    int
	alertErr  error
	onDeliver func() error
}

func (d *outboxDeliverer) Deliver(_ context.Context, v Delivery) error {
	d.calls = append(d.calls, v)
	if d.onDeliver != nil {
		if e := d.onDeliver(); e != nil {
			return e
		}
	}
	return d.fail[v.Target.Channel]
}
func (d *outboxDeliverer) AlertDelivery(context.Context) error { d.alerts++; return d.alertErr }

type outboxPermanent struct{}

func (outboxPermanent) Error() string           { return "refused" }
func (outboxPermanent) DeliveryPermanent() bool { return true }
func outboxFixture(t *testing.T, d *outboxDeliverer) (*Scheduler, *Job, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	s := New(Options{Home: t.TempDir(), Deliver: d, Now: func() time.Time { return now }})
	t.Cleanup(s.cancelRuns)
	for _, p := range []string{s.state, s.tmp} {
		if e := os.MkdirAll(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	j := NewJob()
	j.Set("id", "report")
	return s, j, &now
}
func TestOutboxPartialSuccessRestartOnlyUnsent(t *testing.T) {
	d := &outboxDeliverer{fail: map[string]error{"b": errors.New("temporary")}}
	s, j, now := outboxFixture(t, d)
	if got := s.enqueueDelivery(context.Background(), j, outcome{text: "retained"}, []Target{{Platform: "slack", Channel: "a"}, {Platform: "slack", Channel: "b"}}); got == "" {
		t.Fatal("expected retained failure")
	}
	if len(d.calls) != 2 || d.alerts != 1 {
		t.Fatalf("calls=%d alerts=%d", len(d.calls), d.alerts)
	}
	d.fail = nil
	*now = now.Add(time.Hour)
	restarted := New(s.o)
	t.Cleanup(restarted.cancelRuns)
	restarted.recoverDeliveries()
	if len(d.calls) != 3 || d.calls[2].Target.Channel != "b" || d.calls[2].Text != "retained" {
		t.Fatalf("unexpected replay: %#v", d.calls)
	}
	if restarted.deliveryBlocked(j.ID()) {
		t.Fatal("completed delivery still blocks job")
	}
}
func TestOutboxPermanentFailureAlertOnceAndExplicitRepair(t *testing.T) {
	d := &outboxDeliverer{fail: map[string]error{"bad": outboxPermanent{}}}
	s, j, now := outboxFixture(t, d)
	s.enqueueDelivery(context.Background(), j, outcome{text: "retained"}, []Target{{Platform: "slack", Channel: "bad"}})
	for range 3 {
		*now = now.Add(time.Hour)
		s.recoverDeliveries()
	}
	if len(d.calls) != 1 || d.alerts != 1 {
		t.Fatalf("blocked delivery retried/alert repeated: calls=%d alerts=%d", len(d.calls), d.alerts)
	}
	lane := "slack:fixed"
	s.SetTargetCheck(func(v Target) error {
		if v.Channel != "fixed" {
			return errors.New("policy")
		}
		return nil
	})
	if _, e := s.deliveryAction(cronArgs{Action: "retry_delivery", JobID: j.ID(), Deliver: &lane}, ""); e != nil {
		t.Fatal(e)
	}
	s.recoverDeliveries()
	if len(d.calls) != 2 || d.calls[1].Trusted || d.calls[1].Target.Channel != "fixed" {
		t.Fatalf("repair did not use current policy: %#v", d.calls)
	}
}
func TestOutboxCorruptStoreFailsClosed(t *testing.T) {
	d := &outboxDeliverer{}
	s, j, _ := outboxFixture(t, d)
	p := filepath.Join(s.state, "deliveries-v1.json")
	raw := []byte("{broken")
	if e := os.WriteFile(p, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if !s.deliveryBlocked(j.ID()) {
		t.Fatal("corrupt outbox did not block execution")
	}
	if s.enqueueDelivery(context.Background(), j, outcome{text: "secret"}, []Target{{Platform: "slack"}}) == "" {
		t.Fatal("corrupt store accepted report")
	}
	s.recoverDeliveries()
	if _, e := s.deliveryAction(cronArgs{Action: "discard_delivery", JobID: j.ID()}, ""); e == nil {
		t.Fatal("corrupt store allowed action")
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(raw) || len(d.calls) != 0 {
		t.Fatal("corrupt store was overwritten or forwarded")
	}
}
func TestOutboxQuarantineNeverForwardsOrRetries(t *testing.T) {
	d := &outboxDeliverer{}
	s, j, now := outboxFixture(t, d)
	ctx := messaging.WithUntrusted(context.Background(), "untrusted source")
	s.enqueueDelivery(ctx, j, outcome{text: "private"}, []Target{{Platform: "slack"}})
	*now = now.Add(time.Hour)
	s.recoverDeliveries()
	if len(d.calls) != 0 {
		t.Fatal("quarantined report forwarded")
	}
	lane := "slack:fixed"
	if _, e := s.deliveryAction(cronArgs{Action: "retry_delivery", JobID: j.ID(), Deliver: &lane}, ""); e == nil {
		t.Fatal("quarantined report accepted repair")
	}
	if _, e := s.deliveryAction(cronArgs{Action: "discard_delivery", JobID: j.ID()}, ""); e != nil {
		t.Fatal(e)
	}
	if s.deliveryBlocked(j.ID()) {
		t.Fatal("discard did not unblock job")
	}
}

func TestOutboxRepairRejectedByCurrentPolicyPreservesReport(t *testing.T) {
	d := &outboxDeliverer{fail: map[string]error{"old": outboxPermanent{}}}
	s, j, _ := outboxFixture(t, d)
	s.enqueueDelivery(context.Background(), j, outcome{text: "retained"}, []Target{{Platform: "slack", Channel: "old"}})
	s.SetTargetCheck(func(Target) error { return errors.New("destination denied") })
	lane := "slack:denied"
	if _, e := s.deliveryAction(cronArgs{Action: "retry_delivery", JobID: j.ID(), Deliver: &lane}, ""); e == nil {
		t.Fatal("repair bypassed policy")
	}
	f, e := s.readDeliveries()
	if e != nil {
		t.Fatal(e)
	}
	p := f.Pending[j.ID()]
	if p == nil || !p.Blocked || p.Targets[0].Target.Channel != "old" || len(d.calls) != 1 {
		t.Fatal("rejected repair changed retained report")
	}
}
func TestOutboxPersistenceFailureDoesNotAttemptDelivery(t *testing.T) {
	d := &outboxDeliverer{}
	s, j, _ := outboxFixture(t, d)
	// An existing directory at the destination makes atomic replacement fail.
	if e := os.Mkdir(filepath.Join(s.state, "deliveries-v1.json"), 0700); e != nil {
		t.Fatal(e)
	}
	p := &pendingDelivery{ID: "attempt", JobID: j.ID(), Text: "retained", Targets: []deliveryTarget{{Target: Target{Platform: "slack"}}}}
	f := &deliveryFile{Version: 1, Pending: map[string]*pendingDelivery{j.ID(): p}}
	if got := s.attemptDelivery(context.Background(), f, p); got == "" {
		t.Fatal("expected storage error")
	}
	if len(d.calls) != 0 || d.alerts != 0 {
		t.Fatal("storage failure performed network I/O")
	}
}

func TestOutboxAttemptsAndFailedHomeAlertsAreBounded(t *testing.T) {
	d := &outboxDeliverer{fail: map[string]error{"bad": errors.New("temporary")}, alertErr: errors.New("home unavailable")}
	s, j, now := outboxFixture(t, d)
	s.enqueueDelivery(context.Background(), j, outcome{text: "retained"}, []Target{{Platform: "slack", Channel: "bad"}})
	for range 12 {
		*now = now.Add(2 * time.Hour)
		s.recoverDeliveries()
	}
	f, e := s.readDeliveries()
	if e != nil {
		t.Fatal(e)
	}
	p := f.Pending[j.ID()]
	if len(d.calls) != 5 || p.Attempts != 5 || !p.Blocked {
		t.Fatalf("unbounded retries: calls=%d entry=%+v", len(d.calls), p)
	}
	if d.alerts != 3 || p.AlertAttempts != 3 || p.Alerted {
		t.Fatalf("home alert cap incorrect: alerts=%d entry=%+v", d.alerts, p)
	}
}
func TestOutboxInvalidCountersRefusedOnLoad(t *testing.T) {
	for _, tc := range []struct {
		name             string
		attempts, alerts int
	}{{"negative attempts", -2, 0}, {"negative alerts", 0, -1}, {"excess attempts", 1001, 0}, {"excess alerts", 0, 4}} {
		t.Run(tc.name, func(t *testing.T) {
			d := &outboxDeliverer{}
			s, j, _ := outboxFixture(t, d)
			p := &pendingDelivery{ID: "invalid", JobID: j.ID(), Attempts: tc.attempts, AlertAttempts: tc.alerts, Targets: []deliveryTarget{{Target: Target{Platform: "slack"}}}}
			if e := s.writeDeliveries(&deliveryFile{Version: 1, Pending: map[string]*pendingDelivery{j.ID(): p}}); e != nil {
				t.Fatal(e)
			}
			if _, e := s.readDeliveries(); e == nil {
				t.Fatal("invalid counter accepted")
			}
			if !s.deliveryBlocked(j.ID()) {
				t.Fatal("invalid store did not fail closed")
			}
			s.recoverDeliveries()
			if len(d.calls) != 0 || d.alerts != 0 {
				t.Fatal("invalid store caused network I/O")
			}
		})
	}
}
func TestOutboxDeliverCanReenterCronjobManageList(t *testing.T) {
	d := &outboxDeliverer{}
	s, j, _ := outboxFixture(t, d)
	d.onDeliver = func() error {
		_, e := s.Tool(nil).Run(context.Background(), nil, json.RawMessage(`{"action":"list"}`))
		return e
	}
	done := make(chan string, 1)
	go func() {
		done <- s.enqueueDelivery(context.Background(), j, outcome{text: "report"}, []Target{{Platform: "slack"}})
	}()
	select {
	case result := <-done:
		if result != "" {
			t.Fatalf("reentrant list failed: %s", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("delivery deadlocked calling cronjob_manage list")
	}
	if len(d.calls) != 1 {
		t.Fatalf("calls=%d", len(d.calls))
	}
}
