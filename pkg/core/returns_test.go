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
	s := OnEvent(open(), EventProgress, 1, false, t0)
	wantKinds(t, s, ActEnqueue)
	if a := s.Actions[0]; a.From != 1 || a.To != 1 {
		t.Fatalf("covers %d..%d", a.From, a.To)
	}
	s = OnEvent(s.Next, EventAnswered, 2, false, t0)
	wantKinds(t, s) // one at a time
	s = OnDelivery(s.Next, OutcomePosted, DispositionUpdate, 1, false, t0)
	wantKinds(t, s, ActSchedule, ActEnqueue)
	if a := s.Actions[1]; a.From != 2 || a.To != 2 || s.Next.State != ReturnUpdated {
		t.Fatalf("the waiting event %+v, state %s", a, s.Next.State)
	}
}

// [DONE] closes; an answer alone does not; nothing more is delivered after.
func TestReturnCompletesOnlyByDone(t *testing.T) {
	s := OnEvent(open(), EventAnswered, 1, false, t0)
	s = OnDelivery(s.Next, OutcomePosted, DispositionUpdate, 1, false, t0)
	if s.Next.State != ReturnUpdated {
		t.Fatalf("an answer posted as an update: %s", s.Next.State)
	}
	s = OnEvent(s.Next, EventProgress, 2, false, t0)
	s = OnDelivery(s.Next, OutcomePosted, DispositionDone, 2, false, t0)
	if s.Next.State != ReturnDone {
		t.Fatalf("done: %s", s.Next.State)
	}
	wantKinds(t, OnEvent(s.Next, EventAnswered, 3, false, t0))
	wantKinds(t, OnTick(s.Next, t0.Add(10*time.Hour)))
}

// Told it failed: failed; only its own late answer still goes, once.
func TestReturnFailedTakesOneLateAnswer(t *testing.T) {
	s := OnEvent(open(), EventFailure, 1, false, t0)
	s = OnDelivery(s.Next, OutcomePosted, DispositionUpdate, 1, true, t0)
	if s.Next.State != ReturnFailed {
		t.Fatalf("state %s", s.Next.State)
	}
	wantKinds(t, OnEvent(s.Next, EventProgress, 2, false, t0))
	wantKinds(t, OnEvent(s.Next, EventAnswered, 2, true, t0)) // a child's answer is not its own
	s = OnEvent(s.Next, EventAnswered, 3, false, t0)
	wantKinds(t, s, ActEnqueue)
	s = OnDelivery(s.Next, OutcomePosted, DispositionDone, 3, false, t0)
	if s.Next.State != ReturnDone {
		t.Fatalf("a late answer delivered with [DONE]: %s", s.Next.State)
	}
	f := ReturnView{State: ReturnFailed, LateUsed: 1}
	wantKinds(t, OnEvent(f, EventAnswered, 9, false, t0))
}

// [SILENT] covers the events and schedules the next due time; the state stays.
func TestReturnSilent(t *testing.T) {
	s := OnEvent(open(), EventStatus, 1, false, t0)
	s = OnDelivery(s.Next, OutcomeSilent, DispositionSilent, 1, false, t0)
	wantKinds(t, s, ActSchedule)
	if s.Next.State != ReturnOpen || s.Next.SeqSeen != 1 || s.Next.Active {
		t.Fatalf("%+v", s.Next)
	}
}

// A turn that failed with no side effect runs again, then falls back; one that acted never reruns.
func TestReturnTurnFailures(t *testing.T) {
	s := OnEvent(open(), EventAnswered, 1, false, t0)
	for i := 1; i < ReturnTurnTries; i++ {
		s = OnDelivery(s.Next, OutcomeTurnFailed, "", 1, false, t0)
		wantKinds(t, s, ActRetry)
	}
	s = OnDelivery(s.Next, OutcomeTurnFailed, "", 1, false, t0)
	wantKinds(t, s, ActFallback, ActSchedule)
	if s.Next.Active || s.Next.SeqSeen != 1 {
		t.Fatalf("%+v", s.Next)
	}
	s = OnEvent(open(), EventAnswered, 1, false, t0)
	wantKinds(t, OnDelivery(s.Next, OutcomeSideEffects, "", 1, false, t0), ActFallback, ActSchedule)
	wantKinds(t, OnDelivery(s.Next, OutcomePartial, DispositionUpdate, 1, false, t0), ActFallback, ActSchedule)
}

// An unproven send holds the return; events wait; a person resolves it.
func TestReturnUnknownIsHeldUntilResolved(t *testing.T) {
	s := OnEvent(open(), EventAnswered, 1, false, t0)
	s = OnDelivery(s.Next, OutcomeUnknown, DispositionDone, 1, false, t0)
	wantKinds(t, s, ActFinding)
	if s.Next.State != ReturnUnknown || s.Next.Prev != ReturnOpen {
		t.Fatalf("%+v", s.Next)
	}
	held := s.Next
	wantKinds(t, OnEvent(held, EventProgress, 2, false, t0))  // recorded, not delivered
	wantKinds(t, OnTick(held, t0.Add(30*24*time.Hour)))       // never expires, never escalates
	r := OnResolve(held, true, DispositionDone, 1, false, t0) // it did reach them, with [DONE]
	if r.Next.State != ReturnDone {
		t.Fatalf("resolved sent: %s", r.Next.State)
	}
	r = OnResolve(held, false, DispositionDone, 1, false, t0)
	wantKinds(t, r, ActResend)
	if r.Next.State != ReturnOpen || !r.Next.Active {
		t.Fatalf("resolved resend: %+v", r.Next)
	}
	held.SeqLast = 2
	r = OnResolve(held, true, DispositionUpdate, 1, false, t0)
	wantKinds(t, r, ActSchedule, ActEnqueue) // the event that waited goes next
}

// Children: the parent waits, keeps its deadline, never completes by a child.
func TestReturnChildren(t *testing.T) {
	s, ok := OnChild(open(), t0)
	if !ok || s.Next.State != ReturnWaitingChild || s.Next.Children != 1 {
		t.Fatalf("%+v %v", s.Next, ok)
	}
	w := OnEvent(s.Next, EventAnswered, 1, true, t0)
	wantKinds(t, w, ActEnqueue)
	w = OnDelivery(w.Next, OutcomePosted, DispositionUpdate, 1, false, t0)
	if w.Next.State != ReturnWaitingChild {
		t.Fatalf("a child's answer posted: %s", w.Next.State)
	}
	if d := OnTick(w.Next, w.Next.NextDue.Add(time.Minute)); len(d.Actions) != 1 || d.Actions[0].Event != EventDue {
		t.Fatalf("waiting_child keeps its deadline: %v", kinds(d))
	}
	if c := OnChildClosed(w.Next); c.Next.State != ReturnUpdated {
		t.Fatalf("last child closed: %s", c.Next.State)
	}
	if _, ok := OnChild(ReturnView{State: ReturnOpen, Depth: ReturnChildDepth - 1}, t0); ok {
		t.Error("a chain past its depth")
	}
	if _, ok := OnChild(ReturnView{State: ReturnOpen, Escalated: true}, t0); ok {
		t.Error("a child of an escalated return")
	}
	if _, ok := OnChild(ReturnView{State: ReturnDone}, t0); ok {
		t.Error("a child of a closed return")
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
	wantKinds(t, OnTick(e, t0.Add(3*time.Hour)))                      // latched
	wantKinds(t, OnEvent(e, EventAnswered, 1, false, t0), ActEnqueue) // events still reach the person
	e.LastActivity = t0
	x := OnTick(e, e.EscalatedAt.Add(ReturnExpireAfter)) // seven quiet days after the escalation
	if x.Next.State != ReturnExpired {
		t.Fatalf("quiet after escalation: %s", x.Next.State)
	}
}

// A chat that can no longer be reached: fallback, expired.
func TestReturnRouteGone(t *testing.T) {
	s := OnEvent(open(), EventAnswered, 1, false, t0)
	s = OnDelivery(s.Next, OutcomeRouteGone, "", 1, false, t0)
	wantKinds(t, s, ActFallback, ActFinding)
	if s.Next.State != ReturnExpired {
		t.Fatalf("%s", s.Next.State)
	}
}

// An answer that came while the failure update was in flight is still delivered, once.
func TestReturnAnswerBehindAFailureUpdate(t *testing.T) {
	s := OnEvent(open(), EventFailure, 1, false, t0)
	s = OnEvent(s.Next, EventAnswered, 2, false, t0) // while delivery 1 is in flight
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
	wantKinds(t, OnEvent(e, EventProgress, 1, false, t0.Add(3*time.Hour)))
	a := OnEvent(e, EventAnswered, 1, false, t0.Add(3*time.Hour))
	wantKinds(t, a, ActEnqueue)
	a = OnDelivery(a.Next, OutcomePosted, DispositionUpdate, 1, false, t0.Add(3*time.Hour))
	wantKinds(t, OnEvent(a.Next, EventAnswered, 2, false, t0.Add(4*time.Hour)))
	if x := OnTick(e, e.EscalatedAt.Add(ReturnExpireAfter-time.Minute)); x.Next.State == ReturnExpired {
		t.Fatal("expired before seven quiet days after escalation")
	}
	if x := OnTick(e, e.EscalatedAt.Add(ReturnExpireAfter)); x.Next.State != ReturnExpired {
		t.Fatalf("seven quiet days after escalation: %s", x.Next.State)
	}
}

// A child's failure never fails the parent; a child's answer posted as an update keeps it waiting.
func TestReturnChildOutcomesDoNotCloseTheParent(t *testing.T) {
	s, _ := OnChild(open(), t0)
	f := OnEvent(s.Next, EventFailure, 1, true, t0)
	f = OnDelivery(f.Next, OutcomePosted, DispositionUpdate, 1, false, t0) // own failure: false
	if f.Next.State != ReturnWaitingChild {
		t.Fatalf("a child's failure: %s", f.Next.State)
	}
	if f.Next.AnswerSeq != 0 {
		t.Fatal("a child's answer counted as the request's")
	}
}
