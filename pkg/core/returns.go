package core

import "time"

// Return paths (docs/core.md §5, "Returns"). A request an agent sends over the bus from a person's
// conversation carries that conversation's origin; what comes back about it (an answer, progress,
// the bus's "Not answered", a deadline passing) brings the agent back into that conversation, and
// its update reaches the person. This file is the return's state machine, as pure functions: the
// bus captures events into it in the transaction that creates them, the worker reports each
// delivery's settled outcome, and both carry out the actions it answers. The database, the channel
// and the turns are elsewhere.
//
// Its invariants: an event's delivery is captured once, when it is created, and never handed back;
// every captured event is settled exactly once (by a delivery's outcome, or by the return closing);
// a [DONE] cannot settle what it never saw; an update that may have reached the person is never
// sent again.

// Return states.
const (
	ReturnOpen    = "open"    // no update confirmed since the request
	ReturnUpdated = "updated" // an update was confirmed; not the final one yet
	ReturnDone    = "done"    // a confirmed, whole [DONE] covered everything captured
	ReturnExpired = "expired" // closed without that (its CloseReason says why)
)

// Return event kinds.
const (
	EventAnswered = "answered" // the recipient named the request in answers
	EventProgress = "progress" // another message from the recipient in the request's thread
	EventFailure  = "failure"  // the bus told the sender "Not answered"
	EventDue      = "due"      // the update deadline passed (no message)
)

// Dispositions: how a continuation ends its reply (a control line, stripped before posting).
const (
	DispositionDone   = "done"   // [DONE]: the person has what they asked for
	DispositionUpdate = "update" // [UPDATE], or no valid marker: an update
	DispositionSilent = "silent" // [SILENT]: nothing to tell them
)

// Settled outcomes of a delivery.
const (
	OutcomeConfirmed   = "confirmed"   // posted; WhatsApp's server acknowledged it
	OutcomeUnconfirmed = "unconfirmed" // it may or may not have reached them: never sent again
	OutcomeSilent      = "silent"      // the continuation answered [SILENT]: nothing posted
	OutcomeFailed      = "failed"      // it could not be delivered: the owner is told
	OutcomeClosed      = "closed"      // the return closed before it went
)

// Limits of a return.
const (
	ReturnTurnTries   = 3                  // turns a delivery may take that failed with no side effect
	ReturnChildDepth  = 3                  // returns deep a chain may go (a request sent from an update's turn is one deeper)
	ReturnExpireAfter = 7 * 24 * time.Hour // escalated, caught up and quiet this long: expired
)

// ReturnView is a return as its state machine sees it.
type ReturnView struct {
	State string
	// SeqLast is the newest captured event; SeqSettled the newest one accounted for (a delivery's
	// outcome, or the return closing), not the newest the person received.
	SeqLast, SeqSettled int64
	// Active is whether a delivery is in flight (one at a time), covering ActiveFrom..ActiveTo (an
	// empty range for a due prompt).
	Active               bool
	ActiveFrom, ActiveTo int64
	Depth                int // 0 for a request from a person's message, its parent's + 1 from an update's turn
	Escalated            bool
	// NoDue: an unconfirmed [DONE] stopped the due prompts; a new event starts them again.
	NoDue                                          bool
	EscalateAt, EscalatedAt, NextDue, LastActivity time.Time
	CloseReason                                    string
}

// Terminal is whether the return closed: it captures nothing more.
func (v ReturnView) Terminal() bool { return v.State == ReturnDone || v.State == ReturnExpired }

// Action kinds a step asks for.
const (
	ActEnqueue  = "enqueue"  // a delivery covering events From..To (Event: what brought it)
	ActSchedule = "schedule" // compute the next due time from now
	ActEscalate = "escalate" // the owner told the request is overdue
	ActFinding  = "finding"  // a finding (Rule, Severity, Reason)
	ActClose    = "close"    // the active delivery, if any, settles as closed
)

// Action is one thing a step asks the bus or the worker to do.
type Action struct {
	Kind           string
	From, To       int64
	Event          string
	Rule, Severity string
	Reason         string
}

// Step is a transition's result.
type Step struct {
	Next    ReturnView
	Actions []Action
}

func (s *Step) do(a Action) { s.Actions = append(s.Actions, a) }

// enqueue asks for a delivery of what is not settled yet, unless one is in flight (the next one
// after it covers what came meanwhile).
func (s *Step) enqueue(event string) {
	n := &s.Next
	if n.Active || n.Terminal() || n.SeqLast <= n.SeqSettled && event != EventDue {
		return
	}
	n.Active, n.ActiveFrom, n.ActiveTo = true, n.SeqSettled+1, n.SeqLast
	s.do(Action{Kind: ActEnqueue, From: n.ActiveFrom, To: n.ActiveTo, Event: event})
}

// Capture is a new event of an open return, captured in the transaction that created it.
func Capture(v ReturnView, kind string, seq int64, now time.Time) Step {
	s := Step{Next: v}
	n := &s.Next
	if n.Terminal() {
		return s // the caller never captures for a closed return; nothing to do
	}
	if seq > n.SeqLast {
		n.SeqLast = seq
	}
	n.LastActivity, n.NoDue = now, false
	s.enqueue(kind)
	return s
}

// Tick is the return at now: the escalation latch, quiet expiry, the update deadline.
func Tick(v ReturnView, now time.Time) Step {
	s := Step{Next: v}
	n := &s.Next
	if n.Terminal() {
		return s
	}
	if !n.Escalated && !n.EscalateAt.IsZero() && !now.Before(n.EscalateAt) {
		n.Escalated, n.EscalatedAt = true, now
		s.do(Action{Kind: ActEscalate})
		return s // quiet is counted from here: never escalated and expired in one tick
	}
	quiet := n.LastActivity
	if n.EscalatedAt.After(quiet) {
		quiet = n.EscalatedAt
	}
	if n.Escalated && !n.Active && n.SeqSettled == n.SeqLast && !quiet.IsZero() && now.Sub(quiet) >= ReturnExpireAfter {
		n.State, n.CloseReason = ReturnExpired, "quiet after escalation"
		s.do(Action{Kind: ActFinding, Rule: "return_expired", Severity: "medium", Reason: n.CloseReason})
		return s
	}
	if n.Escalated || n.NoDue || n.Active || n.NextDue.IsZero() || now.Before(n.NextDue) {
		return s
	}
	s.enqueue(EventDue)
	return s
}

// DeliveryResult is a delivery's settled outcome, as the worker reports it: the range it covered,
// its outcome, the continuation's disposition, and whether its reply was cut to fit one message.
type DeliveryResult struct {
	From, To    int64
	Outcome     string
	Disposition string
	Truncated   bool
	Reason      string
}

// Finish settles the active delivery's range with its outcome. One that is not the active
// delivery (stale, or reported twice) changes nothing.
func Finish(v ReturnView, r DeliveryResult, now time.Time) Step {
	s := Step{Next: v}
	n := &s.Next
	if !n.Active || n.Terminal() || r.From != n.ActiveFrom || r.To != n.ActiveTo {
		return s
	}
	n.Active, n.ActiveFrom, n.ActiveTo = false, 0, 0
	if r.To > n.SeqSettled {
		n.SeqSettled = r.To
	}
	n.LastActivity = now
	schedule := true
	switch r.Outcome {
	case OutcomeConfirmed:
		if r.Disposition == DispositionDone && !r.Truncated && r.To >= n.SeqLast {
			n.State = ReturnDone // whole, final, and nothing captured it did not see
			return s
		}
		n.State = ReturnUpdated
	case OutcomeUnconfirmed:
		s.do(Action{Kind: ActFinding, Rule: "return_unconfirmed", Severity: "high", Reason: r.Reason})
		if r.Disposition == DispositionDone {
			n.NoDue, schedule = true, false // never regenerated by a reminder; a new event still comes
		}
	case OutcomeFailed:
		s.do(Action{Kind: ActFinding, Rule: "return_undelivered", Severity: "high", Reason: r.Reason})
	}
	if schedule && !n.Escalated {
		s.do(Action{Kind: ActSchedule})
	}
	s.enqueue(EventProgress) // what came while it was in flight goes next
	return s
}

// Close closes the return with reason (the person's chat gone, the agent's returns off, the
// instance draining): every captured event is settled, the active delivery as closed, the owner
// told. The caller never closes while a send is in flight.
func Close(v ReturnView, reason string, now time.Time) Step {
	s := Step{Next: v}
	n := &s.Next
	if n.Terminal() {
		return s
	}
	if n.Active {
		s.do(Action{Kind: ActClose})
	}
	n.State, n.CloseReason, n.SeqSettled = ReturnExpired, reason, n.SeqLast
	n.Active, n.ActiveFrom, n.ActiveTo, n.LastActivity = false, 0, 0, now
	s.do(Action{Kind: ActFinding, Rule: "return_closed", Severity: "high", Reason: reason})
	return s
}

// MayDelegate is why a request sent from one of the return's update turns may not open a return of
// its own ("" when it may): the return closed or overdue, or the chain too deep. A return it opens
// is a sibling on the same conversation, one deeper, with the same escalation time.
func MayDelegate(v ReturnView, now time.Time) string {
	switch {
	case v.Terminal():
		return "the person's request is closed"
	case v.Escalated || !v.EscalateAt.IsZero() && !now.Before(v.EscalateAt):
		return "the person's request is overdue and with the owner"
	case v.Depth+1 >= ReturnChildDepth:
		return "the chain of requests for this person is too deep"
	}
	return ""
}
