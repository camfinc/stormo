package messaging

import (
	"context"
	"errors"
	"fmt"
)

// Information-flow control (docs/needletail.md §7): a run whose active context holds marked data
// (an unverified author's words, marked records, values or rows) is tainted. A tainted run's
// action is allowed, allowed and kept marked, put to a person for approval, or refused, by the
// action's tier. ApprovalNeeded is how a gate says "approval": the caller (the tool registry, a
// Luau host call, the scheduler) turns it into a request the owner decides in Slack, and the
// runtime, never the model, then runs exactly what was approved.

// ApprovalNeeded is an action a person must approve before the runtime runs it.
type ApprovalNeeded struct {
	// What is the action, as the refusal would name it ("send_message to slack:C0…").
	What string
	// Why is why it needs a person: the run's taint, or the risky-command pattern it matched.
	Why string
	// Trust: approving may also endorse the conversation ("Approve & trust thread"): offered
	// when the approval is needed only because the conversation is tainted.
	Trust bool
}

func (e *ApprovalNeeded) Error() string {
	return fmt.Sprintf("%s needs a person's approval: %s", e.What, e.Why)
}

// NeedsApproval is the gate's answer for an action of the Approval tier in a tainted run.
func NeedsApproval(what, why string) error { return &ApprovalNeeded{What: what, Why: why, Trust: true} }

// NeedsApprovalRisky is the answer for a risky command (approvals.commands), in any run.
func NeedsApprovalRisky(what, why string) error { return &ApprovalNeeded{What: what, Why: why} }

// AsApproval is the ApprovalNeeded err wraps, if any.
func AsApproval(err error) (*ApprovalNeeded, bool) {
	var a *ApprovalNeeded
	if errors.As(err, &a) {
		return a, true
	}
	return nil, false
}

type endorsedKey struct{}

// WithEndorsed marks ctx as the runtime running an approved action (approval id): the approval
// endorses that action, so the run's taint does not hold it back (UntrustedWhy is "" for it, and
// the approval gates pass). Only the approvals service makes one, for exactly the payload a person
// approved; the conversation itself stays tainted unless they also chose to trust it.
func WithEndorsed(ctx context.Context, approval string) context.Context {
	return context.WithValue(ctx, endorsedKey{}, approval)
}

// Endorsed is the approval the run ctx belongs to executes ("" for any other run).
func Endorsed(ctx context.Context) string { s, _ := ctx.Value(endorsedKey{}).(string); return s }

type reasonKey struct{}

// WithReason carries the model's own words before a tool call (the step's assistant text): an
// approval card shows them as the agent's claim, never as fact.
func WithReason(ctx context.Context, reason string) context.Context {
	return context.WithValue(ctx, reasonKey{}, reason)
}

// ReasonOf is the agent's stated reason on ctx ("" when it gave none).
func ReasonOf(ctx context.Context) string { s, _ := ctx.Value(reasonKey{}).(string); return s }

type scheduledKey struct{}

// WithScheduled marks ctx as a scheduled run (a job's turn, gate or script): no person is in the
// loop, so an action that needs approval is refused there instead.
func WithScheduled(ctx context.Context) context.Context {
	return context.WithValue(ctx, scheduledKey{}, true)
}

// Scheduled is whether ctx is a scheduled run's.
func Scheduled(ctx context.Context) bool { b, _ := ctx.Value(scheduledKey{}).(bool); return b }
