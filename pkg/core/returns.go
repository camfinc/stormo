package core

import "time"

// Return paths (docs/core.md §5, "Returns"). A request an agent sends over the bus from a person's
// conversation carries that conversation's origin; whatever comes back about it (an answer,
// progress, a failure, a deadline passing) brings the agent back into that conversation, and its
// update reaches the person. This file is the return's state machine, as a pure function: the bus
// feeds it events and delivery outcomes and carries out the actions it answers. The database, the
// channel and the turns are elsewhere.

// Return states.
const (
	ReturnOpen    = "open"    // no update reached the person since the request
	ReturnUpdated = "updated" // an update reached them; not what they asked for yet
	ReturnDone    = "done"    // the person got what they asked for
	ReturnFailed  = "failed"  // the person was told it failed (a late answer still goes)
	ReturnExpired = "expired" // closed with nothing more to do
	ReturnUnknown = "unknown" // a send could not be proven: held for a person
)

// Return event kinds.
const (
	EventAnswered = "answered" // the recipient named the request in answers
	EventProgress = "progress" // another message from the recipient in the request's thread
	EventAcked    = "acked"    // the recipient acked the request without answering
	EventFailure  = "failure"  // the bus told the sender "Not answered"
	EventStatus   = "status"   // the bus told the sender "Still open" or "Past its due time"
	EventDue      = "due"      // the update deadline passed with no update
)

// Dispositions: how a continuation ends its reply (its last line, stripped before posting).
const (
	DispositionDone   = "done"   // [DONE]: the person has what they asked for
	DispositionUpdate = "update" // [UPDATE], or no marker: an update
	DispositionSilent = "silent" // [SILENT]: nothing to tell them
)

// Delivery outcomes, as the delivery reports them.
const (
	OutcomePosted      = "posted"       // every part sent, with ids
	OutcomeSilent      = "silent"       // the continuation answered [SILENT]: nothing posted
	OutcomeTurnFailed  = "turn_failed"  // the turn failed with no side effect: may run again
	OutcomeSideEffects = "side_effects" // the turn failed after a core tool call: never rerun
	OutcomePartial     = "partial"      // some parts sent, the rest failed for certain
	OutcomeUnknown     = "unknown"      // a part may or may not have been sent
	OutcomeRouteGone   = "route_gone"   // the channel can no longer reach the person
)

// Limits of a return.
const (
	ReturnTurnTries   = 3                  // turns a delivery may take that failed with no side effect
	ReturnChildDepth  = 3                  // returns deep a chain may go (a request sent from an update's turn is one deeper)
	ReturnExpireAfter = 7 * 24 * time.Hour // escalated and quiet this long: expired
	ReturnLateAnswers = 1                  // late answers a failed or escalated return delivers
)

// ReturnView is a return as its state machine sees it.
type ReturnView struct {
	State     string
	Prev      string // the state an unknown came from (resend returns to it)
	SeqLast   int64  // the newest event's sequence
	SeqSeen   int64  // the newest event a finished delivery covered
	Active    bool   // a delivery is in flight (one at a time)
	Attempts  int    // turns the active delivery took
	Depth     int    // 0 for a request from a person's message, its parent's + 1 from an update's turn
	Escalated bool
	LateUsed  int
	// AnswerSeq is the newest of the request's answers: a failed or escalated return still delivers
	// it, once.
	AnswerSeq                                      int64
	EscalateAt, EscalatedAt, NextDue, LastActivity time.Time
}

// Terminal is whether nothing more is delivered (but a late answer of a failed one).
func (v ReturnView) Terminal() bool {
	return v.State == ReturnDone || v.State == ReturnExpired
}

// Action kinds a step asks for.
const (
	ActEnqueue  = "enqueue"  // a delivery covering events From..To (Kind: the newest event's, or due)
	ActRetry    = "retry"    // run the active delivery's turn again
	ActFallback = "fallback" // the events From..To go back to the bus, the agent told there (Reason)
	ActEscalate = "escalate" // a finding and the owner told; no more due prompts
	ActFinding  = "finding"  // a finding (Rule)
	ActResend   = "resend"   // send the held delivery's unsent parts again
	ActSchedule = "schedule" // compute the next due time from now
)

// Action is one thing a step asks the bus to do.
type Action struct {
	Kind     string
	From, To int64
	Event    string
	Reason   string
	Rule     string
}

// Step is a transition's result.
type Step struct {
	Next    ReturnView
	Actions []Action
}

func (s *Step) do(a Action) { s.Actions = append(s.Actions, a) }

// enqueue asks for a delivery of the events the person has not been told about, unless one is in
// flight (it waits: the next delivery after it covers them) or the return is held.
func (s *Step) enqueue(kind string) {
	v := &s.Next
	if v.Active || v.State == ReturnUnknown || v.SeqLast <= v.SeqSeen && kind != EventDue {
		return
	}
	v.Active, v.Attempts = true, 0
	s.do(Action{Kind: ActEnqueue, From: v.SeqSeen + 1, To: v.SeqLast, Event: kind})
}

// Eligible is whether an event of kind can still reach the person through the return: never once
// it is closed; once failed or escalated, only an answer while the late allowance lasts. An event
// that is not stays the bus's (it is not recorded, its delivery not taken).
func Eligible(v ReturnView, kind string) bool {
	switch {
	case v.Terminal():
		return false
	case v.State == ReturnFailed || v.Escalated:
		return kind == EventAnswered && v.LateUsed < ReturnLateAnswers
	}
	return true
}

// OnEvent is a new event of the return.
func OnEvent(v ReturnView, kind string, seq int64, now time.Time) Step {
	s := Step{Next: v}
	n := &s.Next
	if seq > n.SeqLast {
		n.SeqLast = seq
	}
	if kind == EventAnswered && seq > n.AnswerSeq {
		n.AnswerSeq = seq
	}
	n.LastActivity = now
	switch {
	case n.Terminal():
		return s // closed: recorded, nothing delivered
	case n.State == ReturnFailed || n.Escalated:
		// the person was told it failed, or the owner was told it is overdue: only the request's own
		// late answer still goes, once
		s.late()
		return s
	}
	s.enqueue(kind) // held (unknown): recorded, delivered once resolved
	return s
}

// late asks for the request's own answer's delivery, once, when it is not delivered yet.
func (s *Step) late() {
	n := &s.Next
	if n.AnswerSeq > n.SeqSeen && n.LateUsed < ReturnLateAnswers && !n.Active && n.State != ReturnUnknown {
		n.LateUsed++
		s.enqueue(EventAnswered)
	}
}

// OnTick is the return at now: escalation, the update deadline, expiry.
func OnTick(v ReturnView, now time.Time) Step {
	s := Step{Next: v}
	n := &s.Next
	if n.Terminal() || n.State == ReturnUnknown {
		return s
	}
	if !n.Escalated && !n.EscalateAt.IsZero() && !now.Before(n.EscalateAt) && n.State != ReturnFailed {
		n.Escalated, n.EscalatedAt = true, now
		s.do(Action{Kind: ActEscalate})
		return s // quiet is counted from here: never escalated and expired in one tick
	}
	quiet := n.LastActivity
	if n.EscalatedAt.After(quiet) {
		quiet = n.EscalatedAt
	}
	if (n.Escalated || n.State == ReturnFailed) && !n.Active && !quiet.IsZero() && now.Sub(quiet) >= ReturnExpireAfter {
		n.State = ReturnExpired
		s.do(Action{Kind: ActFinding, Rule: "return_expired"})
		return s
	}
	if n.State == ReturnFailed || n.Escalated || n.Active || n.NextDue.IsZero() || now.Before(n.NextDue) {
		return s
	}
	s.enqueue(EventDue)
	return s
}

// OnDelivery is the active delivery's outcome. failure: its covered events include the bus's
// "Not answered" about the request; disposition: the continuation's ([DONE] is the agent saying the
// person has what they asked for, whichever event brought it back).
func OnDelivery(v ReturnView, outcome, disposition string, coveredTo int64, failure bool, now time.Time) Step {
	s := Step{Next: v}
	n := &s.Next
	n.LastActivity = now
	from := v.SeqSeen + 1 // what a fallback gives back: the events this delivery covered
	fallback := func(reason string) { s.do(Action{Kind: ActFallback, From: from, To: coveredTo, Reason: reason}) }
	finished := func() {
		n.Active, n.Attempts = false, 0
		if coveredTo > n.SeqSeen {
			n.SeqSeen = coveredTo
		}
	}
	switch outcome {
	case OutcomePosted:
		finished()
		switch {
		case disposition == DispositionDone:
			n.State = ReturnDone
			return s
		case failure:
			n.State = ReturnFailed
		case n.State != ReturnFailed:
			n.State = ReturnUpdated
		}
		s.do(Action{Kind: ActSchedule})
	case OutcomeSilent:
		finished()
		s.do(Action{Kind: ActSchedule})
	case OutcomeTurnFailed:
		n.Attempts++
		if n.Attempts < ReturnTurnTries {
			s.do(Action{Kind: ActRetry})
			return s
		}
		finished()
		fallback("the update's turn failed")
		s.do(Action{Kind: ActSchedule})
	case OutcomeSideEffects:
		finished()
		fallback("the update's turn may have acted and is never run again")
		s.do(Action{Kind: ActSchedule})
	case OutcomePartial:
		finished()
		fallback("only part of the update reached them")
		s.do(Action{Kind: ActSchedule})
	case OutcomeUnknown:
		n.Active = false
		n.Prev, n.State = n.State, ReturnUnknown
		s.do(Action{Kind: ActFinding, Rule: "return_unknown"})
	case OutcomeRouteGone:
		finished()
		n.State = ReturnExpired
		fallback("the person's chat can no longer be reached")
		s.do(Action{Kind: ActFinding, Rule: "return_undelivered"})
	}
	// events that came while it was in flight go in the next delivery; once failed or escalated,
	// only the request's own late answer
	if !n.Active && !n.Terminal() && n.State != ReturnUnknown && n.SeqLast > n.SeqSeen {
		if n.State == ReturnFailed || n.Escalated {
			s.late()
		} else {
			s.enqueue(EventProgress)
		}
	}
	return s
}

// OnResolve is a person resolving a held (unknown) return's delivery, once its unknown parts are
// marked sent or planned again: complete (every part is sent: the delivery's own outcome applies) or
// not (its unsent parts go: posting resumes, the delivery still active).
func OnResolve(v ReturnView, complete bool, disposition string, coveredTo int64, failure bool, now time.Time) Step {
	if v.State != ReturnUnknown {
		return Step{Next: v}
	}
	back := v
	back.State = v.Prev
	if back.State == "" {
		back.State = ReturnOpen
	}
	back.Prev = ""
	back.Active = true
	if complete {
		return OnDelivery(back, OutcomePosted, disposition, coveredTo, failure, now)
	}
	s := Step{Next: back}
	s.do(Action{Kind: ActResend})
	return s
}

// MayDelegate is why a request sent from one of the return's update turns may not open a return of
// its own ("" when it may): the return closed, failed, held or escalated, or the chain too deep. A
// return it opens is a sibling on the same conversation, one deeper, with the same escalation time.
func MayDelegate(v ReturnView, now time.Time) string {
	switch {
	case v.Terminal() || v.State == ReturnFailed:
		return "the person's request is closed"
	case v.State == ReturnUnknown:
		return "an update to the person is held for review"
	case v.Escalated || !v.EscalateAt.IsZero() && !now.Before(v.EscalateAt):
		return "the person's request is overdue and with the owner"
	case v.Depth+1 >= ReturnChildDepth:
		return "the chain of requests for this person is too deep"
	}
	return ""
}
