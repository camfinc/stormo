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

func result(v ReturnView, outcome, disp string) DeliveryResult {
	return DeliveryResult{From: v.ActiveFrom, To: v.ActiveTo, Outcome: outcome, Disposition: disp}
}

// An event asks for one delivery; events while it is in flight wait for the next, which covers them.
func TestReturnEventsBatchBehindTheActiveDelivery(t *testing.T) {
	s := Capture(open(), EventProgress, 1, t0)
	wantKinds(t, s, ActEnqueue)
	if a := s.Actions[0]; a.From != 1 || a.To != 1 {
		t.Fatalf("covers %d..%d", a.From, a.To)
	}
	first := s.Next
	s = Capture(first, EventAnswered, 2, t0)
	wantKinds(t, s) // one at a time
	s = Finish(s.Next, result(first, OutcomeConfirmed, DispositionUpdate), t0)
	wantKinds(t, s, ActSchedule, ActEnqueue)
	if a := s.Actions[1]; a.From != 2 || a.To != 2 || s.Next.State != ReturnUpdated || s.Next.SeqSettled != 1 {
		t.Fatalf("the waiting event %+v, view %+v", a, s.Next)
	}
}

// The outcome table: what each settled outcome does to the return.
func TestReturnFinishOutcomes(t *testing.T) {
	active := Capture(open(), EventAnswered, 1, t0).Next
	for _, c := range []struct {
		outcome, disp string
		truncated     bool
		state         string
		noDue         bool
		actions       []string
	}{
		{OutcomeConfirmed, DispositionUpdate, false, ReturnUpdated, false, []string{ActSchedule}},
		{OutcomeConfirmed, DispositionDone, false, ReturnDone, false, nil},
		{OutcomeConfirmed, DispositionDone, true, ReturnUpdated, false, []string{ActSchedule}}, // cut short: not final
		{OutcomeSilent, DispositionSilent, false, ReturnOpen, false, []string{ActSchedule}},
		{OutcomeUnconfirmed, DispositionUpdate, false, ReturnOpen, false, []string{ActFinding, ActSchedule}},
		{OutcomeUnconfirmed, DispositionDone, false, ReturnOpen, true, []string{ActFinding}}, // stays open, no reminder
		{OutcomeFailed, DispositionUpdate, false, ReturnOpen, false, []string{ActFinding, ActSchedule}},
	} {
		r := result(active, c.outcome, c.disp)
		r.Truncated = c.truncated
		s := Finish(active, r, t0)
		if s.Next.State != c.state || s.Next.NoDue != c.noDue || s.Next.Active || s.Next.SeqSettled != 1 {
			t.Errorf("%s/%s truncated=%v: %+v", c.outcome, c.disp, c.truncated, s.Next)
			continue
		}
		got := kinds(s)
		same := len(got) == len(c.actions)
		for i := 0; same && i < len(got); i++ {
			same = got[i] == c.actions[i]
		}
		if !same {
			t.Errorf("%s/%s truncated=%v: actions %v, want %v", c.outcome, c.disp, c.truncated, got, c.actions)
		}
	}
}

// [DONE] cannot settle what it never saw: a newer event keeps the return open and goes next.
func TestReturnDoneOnlyWhenCaughtUp(t *testing.T) {
	first := Capture(open(), EventProgress, 1, t0).Next
	s := Capture(first, EventAnswered, 2, t0) // the answer arrives while the progress update runs
	s = Finish(s.Next, result(first, OutcomeConfirmed, DispositionDone), t0)
	if s.Next.State != ReturnUpdated {
		t.Fatalf("closed over an answer it never saw: %s", s.Next.State)
	}
	wantKinds(t, s, ActSchedule, ActEnqueue)
	s = Finish(s.Next, result(s.Next, OutcomeConfirmed, DispositionDone), t0)
	if s.Next.State != ReturnDone || s.Next.SeqSettled != 2 {
		t.Fatalf("caught up: %+v", s.Next)
	}
	wantKinds(t, Capture(s.Next, EventAnswered, 3, t0))
	wantKinds(t, Tick(s.Next, t0.Add(10*time.Hour)))
}

// An unconfirmed [DONE] stops the reminders but not the news: a later answer still goes, and its
// capture starts the reminders again.
func TestReturnUnconfirmedDoneStaysReceptive(t *testing.T) {
	a := Capture(open(), EventAnswered, 1, t0).Next
	s := Finish(a, result(a, OutcomeUnconfirmed, DispositionDone), t0)
	wantKinds(t, Tick(s.Next, t0.Add(time.Hour))) // no due prompt replays it
	c := Capture(s.Next, EventAnswered, 2, t0.Add(time.Hour))
	wantKinds(t, c, ActEnqueue)
	if c.Next.NoDue {
		t.Fatal("a new event leaves the reminders off")
	}
}

// A stale or repeated result changes nothing.
func TestReturnFinishIsOnce(t *testing.T) {
	a := Capture(open(), EventAnswered, 1, t0).Next
	r := result(a, OutcomeConfirmed, DispositionUpdate)
	s := Finish(a, r, t0)
	again := Finish(s.Next, r, t0)
	wantKinds(t, again)
	if again.Next != s.Next {
		t.Fatalf("a repeat changed it: %+v", again.Next)
	}
	wantKinds(t, Finish(a, DeliveryResult{From: 5, To: 6, Outcome: OutcomeConfirmed}, t0))
}

// The deadline asks for an update; escalation latches once, stops the reminders but not the news;
// quiet expiry needs it caught up, nothing in flight and seven quiet days after the escalation.
func TestReturnDeadlinesAndEscalation(t *testing.T) {
	v := open()
	wantKinds(t, Tick(v, t0.Add(44*time.Minute)))
	s := Tick(v, t0.Add(46*time.Minute))
	wantKinds(t, s, ActEnqueue)
	if a := s.Actions[0]; a.Event != EventDue || a.From <= a.To {
		t.Fatalf("a due prompt covers no events: %+v", a)
	}
	s = Tick(v, t0.Add(2*time.Hour))
	wantKinds(t, s, ActEscalate)
	e := s.Next
	wantKinds(t, Tick(e, t0.Add(3*time.Hour)))                                  // latched, no reminder
	wantKinds(t, Capture(e, EventProgress, 1, t0.Add(3*time.Hour)), ActEnqueue) // news still goes
	if x := Tick(e, e.EscalatedAt.Add(ReturnExpireAfter-time.Minute)); x.Next.State == ReturnExpired {
		t.Fatal("expired before seven quiet days")
	}
	if x := Tick(e, e.EscalatedAt.Add(ReturnExpireAfter)); x.Next.State != ReturnExpired {
		t.Fatalf("seven quiet days after escalation: %s", x.Next.State)
	}
	pending := e
	pending.SeqLast = 1 // a captured event not settled yet
	if x := Tick(pending, e.EscalatedAt.Add(30*24*time.Hour)); x.Next.State == ReturnExpired {
		t.Fatal("expired with an event still to settle")
	}
	busy := Capture(e, EventProgress, 1, t0.Add(3*time.Hour)).Next
	if x := Tick(busy, e.EscalatedAt.Add(30*24*time.Hour)); x.Next.State == ReturnExpired {
		t.Fatal("expired with a delivery in flight")
	}
	old := open()
	old.LastActivity = t0.Add(-30 * 24 * time.Hour)
	if x := Tick(old, t0.Add(2*time.Hour)); x.Next.State == ReturnExpired {
		t.Fatal("escalated and expired in one tick")
	}
}

// Closing settles everything captured, closes the active delivery and tells the owner.
func TestReturnClose(t *testing.T) {
	a := Capture(open(), EventProgress, 1, t0)
	b := Capture(a.Next, EventAnswered, 2, t0)
	s := Close(b.Next, "the person's chat can no longer be reached", t0)
	wantKinds(t, s, ActClose, ActFinding)
	if n := s.Next; n.State != ReturnExpired || n.SeqSettled != 2 || n.Active || n.CloseReason == "" {
		t.Fatalf("%+v", n)
	}
	wantKinds(t, Close(s.Next, "again", t0))
	wantKinds(t, Close(open(), "the agent's returns are off", t0), ActFinding)
	wantKinds(t, Finish(s.Next, result(a.Next, OutcomeConfirmed, DispositionUpdate), t0)) // late result after closing
}

// A request from an update's turn opens a sibling return unless the person's request is closed,
// overdue or its chain too deep.
func TestReturnMayDelegate(t *testing.T) {
	if why := MayDelegate(open(), t0); why != "" {
		t.Fatalf("an open return: %s", why)
	}
	for _, v := range []ReturnView{{State: ReturnDone}, {State: ReturnExpired}, {State: ReturnOpen, Escalated: true},
		{State: ReturnOpen, EscalateAt: t0}, {State: ReturnOpen, Depth: ReturnChildDepth - 1}} {
		if MayDelegate(v, t0) == "" {
			t.Errorf("%+v may delegate", v)
		}
	}
}
