package core

import (
	"testing"
	"time"
)

func kinds(s Step) []string {
	out := []string{}
	for _, a := range s.Actions {
		out = append(out, a.Kind)
	}
	return out
}

func wantKinds(t *testing.T, s Step, want ...string) {
	t.Helper()
	got := kinds(s)
	if len(got) != len(want) {
		t.Fatalf("actions %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("actions %v, want %v", got, want)
		}
	}
}

var t0 = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func open() ReturnView {
	return ReturnView{State: ReturnOpen, EscalateAt: t0.Add(2 * time.Hour), NextDue: t0.Add(45 * time.Minute)}
}

// An event asks for one delivery; events while it is in flight wait for the next, which covers them.
func TestReturnEventsBatchBehindTheActiveDelivery(t *testing.T) {
	s := OnEvent(open(), EventProgress, 1, t0)
	wantKinds(t, s, ActEnqueue)
	if a := s.Actions[0]; a.From != 1 || a.To != 1 {
		t.Fatalf("covers %d..%d", a.From, a.To)
	}
	s = OnEvent(s.Next, EventAnswered, 2, t0)
	wantKinds(t, s) // one at a time
	s = OnDelivery(s.Next, OutcomePosted, DispositionUpdate, 1, false, t0)
	wantKinds(t, s, ActSchedule, ActEnqueue)
	if a := s.Actions[1]; a.From != 2 || a.To != 2 || s.Next.State != ReturnUpdated {
		t.Fatalf("the waiting event %+v, state %s", a, s.Next.State)
	}
}

// [DONE] closes; an answer alone does not; nothing more is delivered after.
func TestReturnCompletesOnlyByDone(t *testing.T) {
	s := OnEvent(open(), EventAnswered, 1, t0)
	s = OnDelivery(s.Next, OutcomePosted, DispositionUpdate, 1, false, t0)
	if s.Next.State != ReturnUpdated {
		t.Fatalf("an answer posted as an update: %s", s.Next.State)
	}
	s = OnEvent(s.Next, EventProgress, 2, t0)
	s = OnDelivery(s.Next, OutcomePosted, DispositionDone, 2, false, t0)
	if s.Next.State != ReturnDone {
		t.Fatalf("done: %s", s.Next.State)
	}
	wantKinds(t, OnEvent(s.Next, EventAnswered, 3, t0))
	wantKinds(t, OnTick(s.Next, t0.Add(10*time.Hour)))
}

// Told it failed: failed; only its own late answer still goes, once.
func TestReturnFailedTakesOneLateAnswer(t *testing.T) {
	s := OnEvent(open(), EventFailure, 1, t0)
	s = OnDelivery(s.Next, OutcomePosted, DispositionUpdate, 1, true, t0)
	if s.Next.State != ReturnFailed {
		t.Fatalf("state %s", s.Next.State)
	}
	wantKinds(t, OnEvent(s.Next, EventProgress, 2, t0))
	s = OnEvent(s.Next, EventAnswered, 3, t0)
	wantKinds(t, s, ActEnqueue)
	s = OnDelivery(s.Next, OutcomePosted, DispositionDone, 3, false, t0)
	if s.Next.State != ReturnDone {
		t.Fatalf("a late answer delivered with [DONE]: %s", s.Next.State)
	}
	f := ReturnView{State: ReturnFailed, LateUsed: 1}
	wantKinds(t, OnEvent(f, EventAnswered, 9, t0))
}

// [SILENT] covers the events and schedules the next due time; the state stays.
func TestReturnSilent(t *testing.T) {
	s := OnEvent(open(), EventStatus, 1, t0)
	s = OnDelivery(s.Next, OutcomeSilent, DispositionSilent, 1, false, t0)
	wantKinds(t, s, ActSchedule)
	if s.Next.State != ReturnOpen || s.Next.SeqSeen != 1 || s.Next.Active {
		t.Fatalf("%+v", s.Next)
	}
}

// A turn that failed with no side effect runs again, then falls back; one that acted never reruns.
func TestReturnTurnFailures(t *testing.T) {
	s := OnEvent(open(), EventAnswered, 1, t0)
	for i := 1; i < ReturnTurnTries; i++ {
		s = OnDelivery(s.Next, OutcomeTurnFailed, "", 1, false, t0)
		wantKinds(t, s, ActRetry)
	}
	s = OnDelivery(s.Next, OutcomeTurnFailed, "", 1, false, t0)
	wantKinds(t, s, ActFallback, ActSchedule)
	if s.Next.Active || s.Next.SeqSeen != 1 {
		t.Fatalf("%+v", s.Next)
	}
	s = OnEvent(open(), EventAnswered, 1, t0)
	wantKinds(t, OnDelivery(s.Next, OutcomeSideEffects, "", 1, false, t0), ActFallback, ActSchedule)
	wantKinds(t, OnDelivery(s.Next, OutcomePartial, DispositionUpdate, 1, false, t0), ActFallback, ActSchedule)
}

// An unproven send holds the return; events wait; a person resolves it.
func TestReturnUnknownIsHeldUntilResolved(t *testing.T) {
	s := OnEvent(open(), EventAnswered, 1, t0)
	s = OnDelivery(s.Next, OutcomeUnknown, DispositionDone, 1, false, t0)
	wantKinds(t, s, ActFinding)
	if s.Next.State != ReturnUnknown || s.Next.Prev != ReturnOpen {
		t.Fatalf("%+v", s.Next)
	}
	held := s.Next
	wantKinds(t, OnEvent(held, EventProgress, 2, t0))         // recorded, not delivered
	wantKinds(t, OnTick(held, t0.Add(30*24*time.Hour)))       // never expires, never escalates
	r := OnResolve(held, true, DispositionDone, 1, false, t0) // every part reached them, with [DONE]
	if r.Next.State != ReturnDone {
		t.Fatalf("resolved sent: %s", r.Next.State)
	}
	r = OnResolve(held, false, DispositionDone, 1, false, t0) // parts still to send: posting resumes
	wantKinds(t, r, ActResend)
	if r.Next.State != ReturnOpen || !r.Next.Active {
		t.Fatalf("resolved resend: %+v", r.Next)
	}
	held.SeqLast = 2
	r = OnResolve(held, true, DispositionUpdate, 1, false, t0)
	wantKinds(t, r, ActSchedule, ActEnqueue) // the event that waited goes next
}

// A request from an update's turn opens a sibling return unless the person's request is closed,
// held, overdue or its chain too deep.
func TestReturnMayDelegate(t *testing.T) {
	if why := MayDelegate(open(), t0); why != "" {
		t.Fatalf("an open return: %s", why)
	}
	for _, v := range []ReturnView{{State: ReturnDone}, {State: ReturnFailed}, {State: ReturnUnknown}, {State: ReturnOpen, Escalated: true},
		{State: ReturnOpen, EscalateAt: t0}, {State: ReturnOpen, Depth: ReturnChildDepth - 1}} {
		if MayDelegate(v, t0) == "" {
			t.Errorf("%+v may delegate", v)
		}
	}
}

// Closed returns take no event; failed or escalated ones only an answer, while the allowance lasts.
func TestReturnEligible(t *testing.T) {
	for _, k := range []string{EventAnswered, EventProgress, EventAcked, EventFailure, EventStatus} {
		if !Eligible(open(), k) || Eligible(ReturnView{State: ReturnDone}, k) || Eligible(ReturnView{State: ReturnExpired}, k) {
			t.Errorf("%s", k)
		}
	}
	for _, v := range []ReturnView{{State: ReturnFailed}, {State: ReturnUpdated, Escalated: true}} {
		if !Eligible(v, EventAnswered) || Eligible(v, EventProgress) {
			t.Errorf("%+v", v)
		}
		v.LateUsed = ReturnLateAnswers
		if Eligible(v, EventAnswered) {
			t.Errorf("%+v: the late answer twice", v)
		}
	}
}

// A fallback gives back exactly the events the failed delivery covered.
func TestReturnFallbackCoversTheDelivery(t *testing.T) {
	s := OnEvent(open(), EventAnswered, 1, t0)
	s = OnDelivery(s.Next, OutcomePosted, DispositionUpdate, 1, false, t0)
	s = OnEvent(s.Next, EventProgress, 2, t0)
	s = OnEvent(s.Next, EventProgress, 3, t0) // waits behind delivery 2..2
	s = OnDelivery(s.Next, OutcomePartial, DispositionUpdate, 2, false, t0)
	wantKinds(t, s, ActFallback, ActSchedule, ActEnqueue)
	if a := s.Actions[0]; a.From != 2 || a.To != 2 {
		t.Fatalf("fallback %+v", a)
	}
	if a := s.Actions[2]; a.From != 3 || a.To != 3 {
		t.Fatalf("next delivery %+v", a)
	}
}

// The deadline asks for an update; escalation latches once and stops it; quiet expires.
func TestReturnDeadlinesAndEscalation(t *testing.T) {
	v := open()
	wantKinds(t, OnTick(v, t0.Add(44*time.Minute)))
	s := OnTick(v, t0.Add(46*time.Minute))
	wantKinds(t, s, ActEnqueue)
	if s.Actions[0].Event != EventDue {
		t.Fatalf("%+v", s.Actions[0])
	}
	s = OnTick(v, t0.Add(2*time.Hour))
	wantKinds(t, s, ActEscalate) // escalated: no due prompt with it
	e := s.Next
	wantKinds(t, OnTick(e, t0.Add(3*time.Hour)))               // latched
	wantKinds(t, OnEvent(e, EventAnswered, 1, t0), ActEnqueue) // events still reach the person
	e.LastActivity = t0
	x := OnTick(e, e.EscalatedAt.Add(ReturnExpireAfter)) // seven quiet days after the escalation
	if x.Next.State != ReturnExpired {
		t.Fatalf("quiet after escalation: %s", x.Next.State)
	}
}

// A chat that can no longer be reached: fallback, expired.
func TestReturnRouteGone(t *testing.T) {
	s := OnEvent(open(), EventAnswered, 1, t0)
	s = OnDelivery(s.Next, OutcomeRouteGone, "", 1, false, t0)
	wantKinds(t, s, ActFallback, ActFinding)
	if s.Next.State != ReturnExpired {
		t.Fatalf("%s", s.Next.State)
	}
}

// An answer that came while the failure update was in flight is still delivered, once.
func TestReturnAnswerBehindAFailureUpdate(t *testing.T) {
	s := OnEvent(open(), EventFailure, 1, t0)
	s = OnEvent(s.Next, EventAnswered, 2, t0) // while delivery 1 is in flight
	wantKinds(t, s)
	s = OnDelivery(s.Next, OutcomePosted, DispositionUpdate, 1, true, t0)
	wantKinds(t, s, ActSchedule, ActEnqueue)
	if s.Next.State != ReturnFailed || s.Actions[1].Event != EventAnswered || s.Next.LateUsed != 1 {
		t.Fatalf("%+v %+v", s.Next, s.Actions)
	}
}

// Escalated returns deliver the request's own late answer once, nothing else; quiet is counted from
// the escalation, never escalated and expired in one tick.
func TestReturnEscalatedLateAnswerAndExpiry(t *testing.T) {
	v := open()
	v.LastActivity = t0.Add(-30 * 24 * time.Hour)
	s := OnTick(v, t0.Add(2*time.Hour))
	wantKinds(t, s, ActEscalate)
	if s.Next.State == ReturnExpired {
		t.Fatal("escalated and expired in one tick")
	}
	e := s.Next
	wantKinds(t, OnEvent(e, EventProgress, 1, t0.Add(3*time.Hour)))
	a := OnEvent(e, EventAnswered, 1, t0.Add(3*time.Hour))
	wantKinds(t, a, ActEnqueue)
	a = OnDelivery(a.Next, OutcomePosted, DispositionUpdate, 1, false, t0.Add(3*time.Hour))
	wantKinds(t, OnEvent(a.Next, EventAnswered, 2, t0.Add(4*time.Hour)))
	if x := OnTick(e, e.EscalatedAt.Add(ReturnExpireAfter-time.Minute)); x.Next.State == ReturnExpired {
		t.Fatal("expired before seven quiet days after escalation")
	}
	if x := OnTick(e, e.EscalatedAt.Add(ReturnExpireAfter)); x.Next.State != ReturnExpired {
		t.Fatalf("seven quiet days after escalation: %s", x.Next.State)
	}
}
