package sched

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	// notices are the alert calls: the jobs each said, and whether it updated a posted one
	notices []DeliveryAlert
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
func (d *outboxDeliverer) AlertDeliveries(_ context.Context, a DeliveryAlert) (AlertRef, error) {
	d.alerts++
	d.notices = append(d.notices, a)
	if d.alertErr != nil {
		return AlertRef{}, d.alertErr
	}
	if a.Ref.TS != "" {
		return a.Ref, nil
	}
	return AlertRef{Channel: "home", TS: "1.1"}, nil
}

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
	// a transient failure is retried before anyone is alerted
	if len(d.calls) != 2 || d.alerts != 0 {
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
	s.o.Owners = func() []string { return []string{"fixed"} } // a repair goes to its audience, home or an owner's DM
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
	// the notice says it recovered, in place
	if d.alerts != 2 || len(d.notices[1].JobIDs) != 0 || d.notices[1].Ref.TS == "" {
		t.Fatalf("no recovered update: %+v", d.notices)
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
	lane := "slack:old"
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
	if d.alerts != 3 || f.Alert == nil || f.Alert.Attempts != 3 || p.Alerted {
		t.Fatalf("home alert cap incorrect: alerts=%d entry=%+v alert=%+v", d.alerts, p, f.Alert)
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

// Reports waiting for review share ONE home notice: posted for the first, updated in place with
// the count for the next, updated to "recovered" when none waits; a transient failure never
// alerts before its retries are spent; an explicit retry counts the report again.
func TestOutboxOneMergedNotice(t *testing.T) {
	d := &outboxDeliverer{fail: map[string]error{"bad": outboxPermanent{}, "flaky": errors.New("temporary")}}
	s, a, now := outboxFixture(t, d)
	b := NewJob()
	b.Set("id", "second")
	c := NewJob()
	c.Set("id", "third")
	s.enqueueDelivery(context.Background(), a, outcome{text: "one"}, []Target{{Platform: "slack", Channel: "bad"}})
	s.enqueueDelivery(context.Background(), b, outcome{text: "two"}, []Target{{Platform: "slack", Channel: "bad"}})
	s.enqueueDelivery(context.Background(), c, outcome{text: "three"}, []Target{{Platform: "slack", Channel: "flaky"}})
	if len(d.notices) != 2 || d.notices[0].Ref.TS != "" || d.notices[1].Ref.TS == "" || len(d.notices[1].JobIDs) != 2 {
		t.Fatalf("not one notice updated in place: %+v", d.notices)
	}
	f, _ := s.readDeliveries()
	if f.Pending["report"].ErrorClass != ClassNotInChannel || f.Pending["third"].ErrorClass != ClassTransient || f.Pending["third"].Blocked {
		t.Fatalf("classes %+v %+v", f.Pending["report"], f.Pending["third"])
	}
	// the flaky one, retried to exhaustion, joins the same notice
	for range 8 {
		*now = now.Add(2 * time.Hour)
		s.recoverDeliveries()
	}
	last := d.notices[len(d.notices)-1]
	if len(last.JobIDs) != 3 || last.Ref.TS == "" {
		t.Fatalf("the exhausted report did not join the notice: %+v", d.notices)
	}
	// an explicit retry counts it again (not alerted until it fails again)
	if _, e := s.deliveryAction(cronArgs{Action: "retry_delivery", JobID: "third"}, ""); e != nil {
		t.Fatal(e)
	}
	f, _ = s.readDeliveries()
	if f.Pending["third"].Alerted || f.Pending["third"].Blocked {
		t.Fatalf("retry did not reset: %+v", f.Pending["third"])
	}
	// all delivered: recovered
	d.fail = nil
	for _, id := range []string{"report", "second"} {
		if _, e := s.deliveryAction(cronArgs{Action: "retry_delivery", JobID: id}, ""); e != nil {
			t.Fatal(e)
		}
	}
	*now = now.Add(time.Hour)
	s.recoverDeliveries()
	last = d.notices[len(d.notices)-1]
	if len(last.JobIDs) != 0 || last.Ref.TS == "" {
		t.Fatalf("no recovered update: %+v", d.notices)
	}
	if f, _ := s.readDeliveries(); f.Alert != nil {
		t.Fatalf("the notice state outlived recovery: %+v", f.Alert)
	}
}

// A policy refusal and an unsupported platform are permanent: one attempt, classed; a shutdown
// during a delivery is not a failure (no attempt counted, nothing alerted), and recovery does not
// run once the scheduler stops.
func TestOutboxPermanentClassesAndShutdown(t *testing.T) {
	d := &outboxDeliverer{fail: map[string]error{"refused": messaging.Deferred("scheduled Slack delivery", "marked"), "tg": ErrUnsupportedPlatform}}
	s, j, now := outboxFixture(t, d)
	s.enqueueDelivery(context.Background(), j, outcome{text: "kept"}, []Target{{Platform: "slack", Channel: "refused"}})
	for range 3 {
		*now = now.Add(2 * time.Hour)
		s.recoverDeliveries()
	}
	f, _ := s.readDeliveries()
	if p := f.Pending[j.ID()]; p.Attempts != 1 || !p.Blocked || p.ErrorClass != ClassPolicyRefused || len(d.calls) != 1 {
		t.Fatalf("a policy refusal retried: %+v calls %d", p, len(d.calls))
	}
	if v, _ := s.deliveryViews(); v[j.ID()]["error_class"] != ClassPolicyRefused {
		t.Errorf("views %v", v)
	}
	k := NewJob()
	k.Set("id", "tgjob")
	s.enqueueDelivery(context.Background(), k, outcome{text: "kept"}, []Target{{Platform: "slack", Channel: "tg"}})
	f, _ = s.readDeliveries()
	if p := f.Pending["tgjob"]; !p.Blocked || p.ErrorClass != ClassUnsupportedPlatform {
		t.Fatalf("unsupported platform %+v", p)
	}

	// a shutdown cancels a delivery under way: not a failure
	d2 := &outboxDeliverer{}
	s2, j2, _ := outboxFixture(t, d2)
	ctx, cancel := context.WithCancel(context.Background())
	d2.onDeliver = func() error { cancel(); return context.Canceled }
	if got := s2.enqueueDelivery(ctx, j2, outcome{text: "kept"}, []Target{{Platform: "slack", Channel: "x"}}); !strings.Contains(got, "interrupted") {
		t.Fatalf("enqueue %q", got)
	}
	f2, _ := s2.readDeliveries()
	if p := f2.Pending[j2.ID()]; p.Attempts != 0 || p.Failed || p.Blocked || d2.alerts != 0 {
		t.Fatalf("a shutdown counted as a failure: %+v alerts %d", p, d2.alerts)
	}
	s2.setStopping()
	d2.onDeliver = nil
	s2.recoverDeliveries()
	if len(d2.calls) != 1 {
		t.Fatalf("recovery ran while stopping: %d calls", len(d2.calls))
	}
}

// A repair goes to the report's own audience (its targets' channels, any thread), the home channel
// or an owner's DM, never a new one.
func TestOutboxRetargetLimited(t *testing.T) {
	d := &outboxDeliverer{fail: map[string]error{"C0OLD00001": outboxPermanent{}}}
	s, j, _ := outboxFixture(t, d)
	s.o.Owners = func() []string { return []string{"U0OWNER001"} }
	s.enqueueDelivery(context.Background(), j, outcome{text: "kept"}, []Target{{Platform: "slack", Channel: "C0OLD00001", Thread: "1.1"}})
	for _, lane := range []string{"slack:C0ELSEWHE1", "slack:U0STRANGER", "slack:U0OWNER001:1.2"} {
		if _, e := s.deliveryAction(cronArgs{Action: "retry_delivery", JobID: j.ID(), Deliver: &lane}, ""); e == nil {
			t.Errorf("retarget to %s accepted", lane)
		}
	}
	for _, lane := range []string{"slack", "slack:C0OLD00001", "slack:C0OLD00001:1.9", "slack:U0OWNER001"} {
		if _, e := s.deliveryAction(cronArgs{Action: "retry_delivery", JobID: j.ID(), Deliver: &lane}, ""); e != nil {
			t.Errorf("retarget to %s refused: %v", lane, e)
		}
	}
}

// Item 17 (Codex 8): a posted notice whose update keeps failing is tried at most 3 times per set of
// jobs: the set it tried counts the tries, not the set it last showed (which a failure keeps).
func TestOutboxFailedNoticeUpdatesAreBounded(t *testing.T) {
	d := &outboxDeliverer{fail: map[string]error{"bad": outboxPermanent{}}}
	s, a, now := outboxFixture(t, d)
	s.enqueueDelivery(context.Background(), a, outcome{text: "one"}, []Target{{Platform: "slack", Channel: "bad"}})
	if len(d.notices) != 1 {
		t.Fatalf("no first notice: %+v", d.notices)
	}
	d.alertErr = errors.New("home unavailable")
	b := NewJob()
	b.Set("id", "second")
	s.enqueueDelivery(context.Background(), b, outcome{text: "two"}, []Target{{Platform: "slack", Channel: "bad"}})
	before := d.alerts
	for range 10 {
		*now = now.Add(2 * time.Hour)
		s.recoverDeliveries()
	}
	if tries := d.alerts - before; tries > 3 {
		t.Fatalf("the failed update was tried %d times since the set changed (at most 3)", tries+1)
	}
	// a further change of the set is tried again
	c := NewJob()
	c.Set("id", "third")
	d.alertErr = nil
	s.enqueueDelivery(context.Background(), c, outcome{text: "three"}, []Target{{Platform: "slack", Channel: "bad"}})
	if last := d.notices[len(d.notices)-1]; len(last.JobIDs) != 3 {
		t.Fatalf("a new set was not tried: %+v", last)
	}
}
