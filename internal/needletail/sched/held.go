package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/approvals"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// A tainted scheduled run (its context took in marked data: a marked record, value or
// conversation, an untrusted job) keeps its schedule, but its report is held: the owner gets an
// approval card with exactly that report, Approve sends it to the job's targets, Deny (or no
// decision in approvals.delivery_expiry_days) drops it (docs/needletail.md §4.7, §7). A job that
// delivers nowhere (local) holds nothing: its report is its run record.

// DeliveryAsker puts a held report to the approvers: the request's id.
type DeliveryAsker func(ctx context.Context, d approvals.Delivery, why string) (string, error)

// SetApprovals gives the scheduler the approvals that take its tainted runs' reports (serve's);
// nil: such a report is not delivered (its record keeps it).
func (s *Scheduler) SetApprovals(ask DeliveryAsker) { s.askDelivery = ask }

// lane is a target as Targets reads it back.
func lane(t Target) string {
	s := t.Platform
	if t.Channel != "" {
		s += ":" + t.Channel
		if t.Thread != "" {
			s += ":" + t.Thread
		}
	}
	return s
}

// hold puts a tainted run's report to the approvers instead of delivering it: the public note the
// job keeps (never the report, its targets or the taint's words).
func (s *Scheduler) hold(ctx context.Context, j *Job, o outcome, targets []Target) string {
	d := approvals.Delivery{JobID: j.ID(), JobName: j.Name(), Session: o.session, Text: o.text, Trusted: s.trusted(j), Failure: o.failed()}
	for _, t := range targets {
		d.Targets = append(d.Targets, lane(t))
	}
	if s.askDelivery == nil {
		return "The run read untrusted data, so its report was not delivered (approvals are not available); it is kept in the run's private record."
	}
	id, err := s.askDelivery(ctx, d, messaging.UntrustedWhy(ctx))
	if err != nil {
		s.logf("job %s: the tainted run's report could not be put to the owner: %s", j.ID(), s.o.Env.Redact(err.Error()))
		return "The run read untrusted data, so its report was not delivered and could not be put to the owner for approval; it is kept in the run's private record."
	}
	return fmt.Sprintf("The run read untrusted data: its report waits for the owner's approval (%s).", id)
}

// DeliverApproved sends a held report the owner approved (approvals.KindDelivery): exactly its
// text to its job's targets, under ctx (endorsed). Its answer is what was sent.
func (s *Scheduler) DeliverApproved(ctx context.Context, r *approvals.Request) (string, error) {
	d := r.Action.Delivery
	if d == nil {
		return "", errors.New("no report")
	}
	targets, err := Targets(strings.Join(d.Targets, ","), json.RawMessage(nil))
	if err != nil {
		return "", err
	}
	var failed []string
	sent := 0
	for _, t := range targets {
		if err := s.o.Deliver.Deliver(ctx, Delivery{JobID: d.JobID, JobName: d.JobName, Target: t, Text: d.Text, Failure: d.Failure, Session: d.Session, Trusted: d.Trusted}); err != nil {
			failed = append(failed, t.String()+": "+s.o.Env.Redact(err.Error()))
			continue
		}
		sent++
	}
	if len(failed) > 0 {
		return "", fmt.Errorf("sent to %d of %d targets; %s", sent, len(targets), strings.Join(failed, "; "))
	}
	return fmt.Sprintf("the report was sent to %d target(s)", sent), nil
}

// JobJSON is a job's definition as cron/jobs.json holds it (an approval card's "before").
func (s *Scheduler) JobJSON(id string) (json.RawMessage, bool) {
	var out json.RawMessage
	_ = s.mutate(func(f *File) (bool, error) {
		if j := f.Find(id); j != nil {
			out = j.encode()
		}
		return false, nil
	})
	return out, out != nil
}
