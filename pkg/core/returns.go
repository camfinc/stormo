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
	ReturnOpen         = "open"          // no update reached the person since the request
	ReturnUpdated      = "updated"       // an update reached them; not what they asked for yet
	ReturnWaitingChild = "waiting_child" // the agent asked someone else on the person's behalf
	ReturnDone         = "done"          // the person got what they asked for
	ReturnFailed       = "failed"        // the person was told it failed (a late answer still goes)
	ReturnExpired      = "expired"       // closed with nothing more to do
	ReturnUnknown      = "unknown"       // a send could not be proven: held for a person
)

// Return event kinds (of the request, or of a child request).
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
	ReturnChildDepth  = 3                  // children deep a return's chain may go
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
	Depth     int    // 0 for a return from a person's conversation, its parent's + 1 for a child
	Children  int    // open children
	Escalated bool
	LateUsed  int
	// AnswerSeq is the newest of the request's own answers (a child's never counts): a failed or
	// escalated return still delivers it, once.
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
	ActFallback = "fallback" // wake the agent in the bus thread instead (Reason)
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

// OnEvent is a new event of the return (child: an event of one of its children's requests).
func OnEvent(v ReturnView, kind string, seq int64, child bool, now time.Time) Step {
	s := Step{Next: v}
	n := &s.Next
	if seq > n.SeqLast {
		n.SeqLast = seq
	}
	if kind == EventAnswered && !child && seq > n.AnswerSeq {
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
// "Not answered" about the request itself (a child's failure never fails the parent: the caller
// passes only the request's own); disposition: the continuation's ([DONE] is the agent saying the
// person has what they asked for, whichever event brought it back).
func OnDelivery(v ReturnView, outcome, disposition string, coveredTo int64, failure bool, now time.Time) Step {
	s := Step{Next: v}
	n := &s.Next
	n.LastActivity = now
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
		case n.Children > 0:
			n.State = ReturnWaitingChild
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
		s.do(Action{Kind: ActFallback, Reason: "the update's turn failed"})
		s.do(Action{Kind: ActSchedule})
	case OutcomeSideEffects:
		finished()
		s.do(Action{Kind: ActFallback, Reason: "the update's turn stopped after it had acted"})
		s.do(Action{Kind: ActSchedule})
	case OutcomePartial:
		finished()
		s.do(Action{Kind: ActFallback, Reason: "only part of the update reached them"})
		s.do(Action{Kind: ActSchedule})
	case OutcomeUnknown:
		n.Active = false
		n.Prev, n.State = n.State, ReturnUnknown
		s.do(Action{Kind: ActFinding, Rule: "return_unknown"})
	case OutcomeRouteGone:
		finished()
		n.State = ReturnExpired
		s.do(Action{Kind: ActFallback, Reason: "the person's chat can no longer be reached"})
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

// OnResolve is a person resolving a held (unknown) return's delivery: sent (it reached them: the
// delivery's own outcome applies) or resend (its unsent parts go again).
func OnResolve(v ReturnView, sent bool, disposition string, coveredTo int64, failure bool, now time.Time) Step {
	if v.State != ReturnUnknown {
		return Step{Next: v}
	}
	back := v
	back.State = v.Prev
	if back.State == "" {
		back.State = ReturnOpen
	}
	back.Prev = ""
	if sent {
		back.Active = true
		return OnDelivery(back, OutcomePosted, disposition, coveredTo, failure, now)
	}
	s := Step{Next: back}
	s.Next.Active = true
	s.do(Action{Kind: ActResend})
	return s
}

// OnChild is the agent asking someone else from the return's continuation: refused (false) when the
// return is closed, held, escalated or its chain is too deep.
func OnChild(v ReturnView, now time.Time) (Step, bool) {
	s := Step{Next: v}
	n := &s.Next
	if n.Terminal() || n.State == ReturnFailed || n.State == ReturnUnknown || n.Escalated || n.Depth+1 >= ReturnChildDepth ||
		!n.EscalateAt.IsZero() && !now.Before(n.EscalateAt) {
		return s, false
	}
	n.Children++
	if !n.Active {
		n.State = ReturnWaitingChild
	}
	return s, true
}

// OnChildClosed is one of the return's children closing.
func OnChildClosed(v ReturnView) Step {
	s := Step{Next: v}
	n := &s.Next
	if n.Children > 0 {
		n.Children--
	}
	if n.Children == 0 && n.State == ReturnWaitingChild {
		n.State = ReturnUpdated
	}
	return s
}
