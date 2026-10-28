package sched

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// DeliveryAlerter keeps ONE runtime notice in the configured home about the reports waiting for
// review: posted when the first one is, updated in place (Ref) as the set changes, and updated to
// say delivery recovered once none waits. It carries a count and opaque job ids only, never
// report content, targets or errors, and runs no user/model handler.
type DeliveryAlerter interface {
	AlertDeliveries(ctx context.Context, a DeliveryAlert) (AlertRef, error)
}

// DeliveryAlert is the home notice's state: the jobs whose reports wait for review (none:
// recovered), and the message to update (Ref; empty: post a new one).
type DeliveryAlert struct {
	JobIDs []string
	Ref    AlertRef
}

// AlertRef is the posted notice.
type AlertRef struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// Coarse delivery error classes (delivery_recovery's error_class): policy_refused,
// unsupported_platform, not_in_channel and auth are permanent (no automatic retry: the report
// waits for review); transient is retried.
const (
	ClassPolicyRefused       = "policy_refused"
	ClassUnsupportedPlatform = "unsupported_platform"
	ClassNotInChannel        = "not_in_channel"
	ClassAuth                = "auth"
	ClassTransient           = "transient"
)

// ErrUnsupportedPlatform is a delivery to a platform the deliverer does not serve (permanent).
var ErrUnsupportedPlatform = errors.New("unsupported delivery platform")

// errorClass is a delivery error's class: a deliverer's own (DeliveryClass), a policy refusal
// (messaging.ErrRefused), an unsupported platform, a permanent provider error, else transient.
func errorClass(e error) string {
	var c interface{ DeliveryClass() string }
	if errors.As(e, &c) {
		if k := c.DeliveryClass(); k != "" {
			return k
		}
	}
	var permanent interface{ DeliveryPermanent() bool }
	switch {
	case errors.Is(e, messaging.ErrRefused):
		return ClassPolicyRefused
	case errors.Is(e, ErrUnsupportedPlatform):
		return ClassUnsupportedPlatform
	case errors.As(e, &permanent) && permanent.DeliveryPermanent():
		return ClassNotInChannel
	}
	return ClassTransient
}

// alertState is the home notice (deliveries-v1.json's "alert", additive): where it is, the jobs it
// counts, and how many tries the current set had (at most 3 until the set changes).
type alertState struct {
	Ref      AlertRef `json:"ref"`
	Jobs     []string `json:"jobs"`
	Attempts int      `json:"attempts,omitempty"`
	// Tried is the set of jobs the last attempts were for (additive): Attempts counts tries of
	// this set, reset only when the set waiting changes from it (Jobs is what the notice shows,
	// which a failed update leaves as it was).
	Tried []string `json:"tried,omitempty"`
}

type deliveryTarget struct {
	Target Target `json:"target"`
	Sent   bool   `json:"sent"`
}
type pendingDelivery struct {
	JobName       string           `json:"job_name,omitempty"`
	ID            string           `json:"id"`
	JobID         string           `json:"job_id"`
	Session       string           `json:"session,omitempty"`
	Text          string           `json:"text"`
	Failure       bool             `json:"failure"`
	Trusted       bool             `json:"trusted"`
	Quarantined   bool             `json:"quarantined"`
	Targets       []deliveryTarget `json:"targets"`
	Attempts      int              `json:"attempts"`
	Failed        bool             `json:"failed"`
	Blocked       bool             `json:"blocked"`
	Alerted       bool             `json:"alerted"`
	AlertAttempts int              `json:"alert_attempts"`
	Next          time.Time        `json:"next"`
	// ErrorClass is the last failure's coarse class (additive): errorClass.
	ErrorClass string `json:"error_class,omitempty"`
	// Audience are the report's original targets (additive): a repair stays within their
	// channels, the home channel or an owner's DM. Absent (an older entry): its targets.
	Audience []Target `json:"audience,omitempty"`
}

func (p *pendingDelivery) complete() bool {
	for _, t := range p.Targets {
		if !t.Sent {
			return false
		}
	}
	return true
}

type deliveryFile struct {
	Version int                         `json:"version"`
	Pending map[string]*pendingDelivery `json:"pending"`
	// Alert is the one home notice about the reports waiting for review (additive; absent: none).
	Alert *alertState `json:"alert,omitempty"`
}

func (s *Scheduler) readDeliveries() (*deliveryFile, error) {
	b, e := os.ReadFile(filepath.Join(s.state, "deliveries-v1.json"))
	if errors.Is(e, os.ErrNotExist) {
		return &deliveryFile{Version: 1, Pending: map[string]*pendingDelivery{}}, nil
	}
	if e != nil {
		return nil, e
	}
	var f deliveryFile
	if e = json.Unmarshal(b, &f); e != nil {
		return nil, e
	}
	if f.Version != 1 || f.Pending == nil || len(f.Pending) > 1000 {
		return nil, errors.New("invalid delivery outbox")
	}
	for id, p := range f.Pending {
		if p == nil || p.JobID != id || p.ID == "" || len(p.Targets) == 0 || len(p.Targets) > 100 || p.Attempts < 0 || p.Attempts > 1000 || p.AlertAttempts < 0 || p.AlertAttempts > 3 {
			return nil, errors.New("invalid delivery entry")
		}
	}
	for _, p := range f.Pending {
		for _, t := range p.Targets {
			if t.Target.Platform == "" {
				return nil, errors.New("invalid delivery target")
			}
		}
	}
	return &f, nil
}
func (s *Scheduler) writeDeliveries(f *deliveryFile) error {
	b, e := json.Marshal(f)
	if e != nil {
		return e
	}
	return fsx.WriteAtomicIn(filepath.Join(s.state, "deliveries-v1.json"), s.tmp, b, 0600)
}

// deliveryMu protects store edits; deliverySendMu serializes attempts. Never hold
// deliveryMu across provider calls: before_send handlers may list/manage jobs.
func (s *Scheduler) deliveryBlocked(id string) bool {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	f, e := s.readDeliveries()
	if e != nil {
		return true
	}
	p := f.Pending[id]
	return p != nil && !p.complete()
}
func (s *Scheduler) enqueueDelivery(ctx context.Context, j *Job, o outcome, targets []Target) string {
	if len(targets) == 0 {
		return ""
	}
	s.deliverySendMu.Lock()
	defer s.deliverySendMu.Unlock()
	s.deliveryMu.Lock()
	s.deliverySending = true
	defer func() { s.deliverySending = false; s.deliveryMu.Unlock() }()
	f, e := s.readDeliveries()
	if e != nil {
		return "Delivery outbox unavailable; review runtime storage."
	}
	if p := f.Pending[j.ID()]; p != nil && !p.complete() {
		return "A previous report is still pending delivery."
	}
	if len(f.Pending) >= 1000 && f.Pending[j.ID()] == nil {
		return "Delivery outbox is full; review runtime storage."
	}
	b := make([]byte, 16)
	if _, e = rand.Read(b); e != nil {
		return "Could not allocate delivery ID."
	}
	p := &pendingDelivery{ID: hex.EncodeToString(b), JobID: j.ID(), JobName: j.Name(), Session: o.session, Text: o.text, Failure: o.failed(), Trusted: s.trusted(j), Quarantined: messaging.UntrustedWhy(ctx) != ""}
	for _, t := range targets {
		p.Targets = append(p.Targets, deliveryTarget{Target: t})
	}
	p.Audience = append([]Target{}, targets...)
	f.Pending[p.JobID] = p
	if e = s.writeDeliveries(f); e != nil {
		return "Could not retain report; delivery was not attempted."
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return s.attemptDelivery(ctx, f, p)
}
func (s *Scheduler) attemptDelivery(ctx context.Context, f *deliveryFile, p *pendingDelivery) string {
	if p.Quarantined {
		p.Blocked = true
	}
	p.Attempts++
	p.Next = s.o.Now().Add(time.Duration(1<<min(p.Attempts, 6)) * time.Minute)
	// Persist the attempt before I/O. Crash recovery is at-least-once, never exactly-once.
	if e := s.writeDeliveries(f); e != nil {
		return "Could not persist delivery attempt."
	}
	var last error
	interrupted := false
	if p.Quarantined {
		last = errors.New("scheduled forwarding was refused because this run is quarantined.")
	} else {
		for i := range p.Targets {
			t := &p.Targets[i]
			if t.Sent {
				continue
			}
			if messaging.UntrustedWhy(ctx) != "" {
				p.Quarantined = true
				p.Blocked = true
				break
			}
			s.deliveryMu.Unlock()
			e := s.o.Deliver.Deliver(ctx, Delivery{JobID: p.JobID, JobName: p.JobName, Target: t.Target, Text: p.Text, Failure: p.Failure, Session: p.Session, Trusted: p.Trusted})
			s.deliveryMu.Lock()
			if e != nil && (ctx.Err() != nil || errors.Is(e, context.Canceled)) {
				// the runtime stopping (or the attempt's own deadline) is not the destination's
				// failure: no attempt counted, nothing alerted; it goes again at the next start
				interrupted = true
				break
			}
			if e != nil {
				last = e
				p.ErrorClass = errorClass(e)
				if p.ErrorClass != ClassTransient {
					p.Blocked = true // a refusal or a missing destination: retrying asks the same
				}
				continue
			}
			t.Sent = true
			if e = s.writeDeliveries(f); e != nil {
				return "Delivery acknowledgement could not be persisted; review before retrying."
			}
		}
	}
	if messaging.UntrustedWhy(ctx) != "" {
		p.Quarantined = true
		p.Blocked = true
	}
	if interrupted && !p.complete() {
		p.Attempts = max(0, p.Attempts-1)
		p.Next = s.o.Now()
		if e := s.writeDeliveries(f); e != nil {
			return "Could not persist an interrupted delivery."
		}
		return "Delivery was interrupted (the runtime is stopping); the kept report goes at the next start."
	}
	if p.complete() {
		p.ErrorClass = ""
		if e := s.writeDeliveries(f); e != nil {
			return "Could not persist completed delivery."
		}
		if p.Failed && !p.Quarantined && s.o.OnDelivery != nil {
			s.o.OnDelivery(DeliveryEvent{ID: p.ID + ":recovered", JobID: p.JobID, Session: p.Session, At: s.o.Now(), Status: "recovered", Failure: p.Failure})
		}
		s.syncAlert(ctx, f)
		return ""
	}
	first := !p.Failed
	p.Failed = true
	if p.Attempts >= 5 {
		p.Blocked = true
	}
	if e := s.writeDeliveries(f); e != nil {
		return "Could not persist delivery failure."
	}
	if first && !p.Quarantined && s.o.OnDelivery != nil {
		s.o.OnDelivery(DeliveryEvent{ID: p.ID + ":failed", JobID: p.JobID, Session: p.Session, At: s.o.Now(), Status: "failed", Failure: p.Failure})
	}
	// the home notice counts reports whose retries are exhausted or whose error is permanent
	s.syncAlert(ctx, f)
	if p.Quarantined {
		return "scheduled forwarding was refused because this run is quarantined."
	}
	if last != nil {
		s.logf("job %s: delivery attempt failed (%s): %s", p.JobID, p.ErrorClass, s.o.Env.Redact(last.Error()))
	}
	return "Report retained pending delivery; review destination and retry_delivery."
}

// waitingReview are the jobs whose reports wait for a person: retries exhausted or a permanent
// error, not yet delivered (sorted).
func waitingReview(f *deliveryFile) []string {
	out := []string{}
	for id, p := range f.Pending {
		if p.Blocked && !p.complete() {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// syncAlert brings the one home notice in line with the reports waiting for review: posted when
// the first waits, updated in place (with the count and job ids) when the set changes, updated to
// "recovered" when none waits any more. A failed post is tried again at the next change or tick,
// at most 3 times per set. deliveryMu is held (and released around the provider call).
func (s *Scheduler) syncAlert(ctx context.Context, f *deliveryFile) {
	a, ok := s.o.Deliver.(DeliveryAlerter)
	if !ok {
		return
	}
	jobs := waitingReview(f)
	cur := f.Alert
	if cur == nil {
		if len(jobs) == 0 {
			return
		}
		cur = &alertState{}
	}
	if slices.Equal(cur.Jobs, jobs) && cur.Ref.TS != "" {
		return // the notice says this already
	}
	if !slices.Equal(cur.Tried, jobs) {
		cur.Attempts, cur.Tried = 0, slices.Clone(jobs) // a new set: tried afresh, at most 3 times
	}
	if cur.Attempts >= 3 {
		return
	}
	if len(jobs) == 0 && cur.Ref.TS == "" {
		f.Alert = nil // nothing was ever posted: nothing to recover
		_ = s.writeDeliveries(f)
		return
	}
	cur.Attempts++
	f.Alert = cur
	if s.writeDeliveries(f) != nil {
		return
	}
	s.deliveryMu.Unlock()
	ref, e := a.AlertDeliveries(ctx, DeliveryAlert{JobIDs: jobs, Ref: cur.Ref})
	s.deliveryMu.Lock()
	if e != nil {
		s.logf("scheduled delivery home notice failed")
		_ = s.writeDeliveries(f)
		return
	}
	for _, id := range jobs {
		if p := f.Pending[id]; p != nil {
			p.Alerted = true
		}
	}
	if len(jobs) == 0 {
		f.Alert = nil // recovered: the next report waiting gets a new notice
	} else {
		cur.Ref, cur.Jobs, cur.Attempts, cur.Tried = ref, jobs, 0, nil
	}
	if e := s.writeDeliveries(f); e != nil {
		s.logf("could not persist delivery notice status")
	}
}

// recoverDeliveries never executes the model or re-analyzes the source material.
func (s *Scheduler) recoverDeliveries() {
	if s.isStopping() {
		return // a shutdown: what is kept goes at the next start
	}
	// a delivery under way (a run's own) holds the lock across provider I/O: this tick skips
	// recovery rather than wait for it
	if !s.deliverySendMu.TryLock() {
		return
	}
	defer s.deliverySendMu.Unlock()
	s.deliveryMu.Lock()
	s.deliverySending = true
	var recovered []string
	defer func() {
		s.deliverySending = false
		s.deliveryMu.Unlock()
		for _, id := range recovered {
			if s.clearDeliveryHold(id) {
				s.deliveryMu.Lock()
				if latest, err := s.readDeliveries(); err == nil {
					if p := latest.Pending[id]; p != nil && p.complete() {
						delete(latest.Pending, id)
						_ = s.writeDeliveries(latest)
					}
				}
				s.deliveryMu.Unlock()
			}
		}
	}()
	f, e := s.readDeliveries()
	if e != nil {
		s.logf("delivery outbox unavailable; scheduled execution blocked")
		return
	}
	ctx, cancel := context.WithTimeout(s.runCtx, 30*time.Second)
	defer cancel()
	attempts := 0
	for _, p := range f.Pending {
		if ctx.Err() != nil || attempts >= 4 {
			break
		}
		if s.isRunning(p.JobID) {
			continue
		}
		if p.complete() {
			recovered = append(recovered, p.JobID)
			continue
		}
		if s.o.Now().Before(p.Next) {
			continue
		}
		attempts++
		if !p.Blocked {
			if s.attemptDelivery(ctx, f, p) == "" {
				recovered = append(recovered, p.JobID)
			}
		} else {
			p.Next = s.o.Now().Add(30 * time.Minute)
			_ = s.writeDeliveries(f)
		}
	}
	if ctx.Err() == nil {
		s.syncAlert(ctx, f) // a notice that failed earlier, or a set that changed meanwhile
	}
}
func (s *Scheduler) deliveryAction(a cronArgs, untrusted string) (any, error) {
	if untrusted != "" {
		return nil, messaging.Deferred("delivery recovery", untrusted)
	}
	if a.JobID == "" {
		return nil, errors.New("job_id is required")
	}
	if s.isRunning(a.JobID) {
		return nil, errors.New("wait for the active run to finish")
	}
	if a.Deliver != nil {
		if err := s.checkLanes(cronArgs{Deliver: a.Deliver}, nil); err != nil {
			return nil, err
		}
	}
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	if s.deliverySending {
		return nil, errors.New("delivery is in progress; retry after it settles")
	}
	f, e := s.readDeliveries()
	if e != nil {
		return nil, e
	}
	p := f.Pending[a.JobID]
	if a.expectReport != "" && (p == nil || ReportHash(p.Text) != a.expectReport) {
		return nil, errors.New("the kept report changed since the approval card showed it: nothing was done; ask again")
	}
	if a.Action == "discard_delivery" {
		delete(f.Pending, a.JobID)
		if e := s.writeDeliveries(f); e != nil {
			return nil, e
		}
		s.syncAlertLater()
		return map[string]any{"discarded": true}, nil
	}
	if p == nil {
		return nil, errors.New("no pending report for this job")
	}
	if p.Quarantined {
		return nil, errors.New("quarantined reports cannot be retried; review privately and discard_delivery")
	}
	if a.Deliver != nil {
		ts, e := Targets(*a.Deliver, nil)
		if e != nil {
			return nil, e
		}
		if len(ts) == 0 {
			return nil, errors.New("retry requires a delivery target")
		}
		// a repair reaches the report's own audience (its targets' channels), the home channel or
		// an owner's DM: never a new audience for a report the run made for another
		for _, t := range ts {
			if !s.retargetAllowed(p, t) {
				return nil, fmt.Errorf("retry_delivery may send a kept report only to its own destinations' channels, the home channel or the owner's DM, not %s", t)
			}
		}
		// Explicit replacement applies only to unsent targets. Already acknowledged targets stay acknowledged.
		old := p.Targets
		p.Targets = nil
		for _, t := range old {
			if t.Sent {
				p.Targets = append(p.Targets, t)
			}
		}
		for _, t := range ts {
			sent := false
			for _, v := range p.Targets {
				if v.Target == t {
					sent = true
				}
			}
			if !sent {
				p.Targets = append(p.Targets, deliveryTarget{Target: t})
			}
		}
		p.Trusted = false // repaired targets always pass the current messaging policy
	}
	p.Blocked = false
	p.Alerted = false // an explicit retry: the notice counts it again should it fail again
	p.AlertAttempts = 0
	p.ErrorClass = ""
	p.Attempts = 0
	p.Next = s.o.Now()
	if e = s.writeDeliveries(f); e != nil {
		return nil, e
	}
	// Queue recovery; tool success is not a delivery receipt.
	return map[string]any{"queued": true, "job_id": a.JobID, "note": "Retained report will be retried on the next scheduler tick."}, nil
}
func (s *Scheduler) deliveryViews() (map[string]map[string]any, error) {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	f, e := s.readDeliveries()
	if e != nil {
		return nil, fmt.Errorf("delivery outbox unavailable: %w", e)
	}
	out := map[string]map[string]any{}
	for id, p := range f.Pending {
		if p.complete() {
			continue
		}
		v := map[string]any{"pending": true, "attempts": p.Attempts, "needs_review": p.Blocked, "home_alert_sent": p.Alerted, "quarantined": p.Quarantined}
		if p.ErrorClass != "" {
			v["error_class"] = p.ErrorClass
		}
		out[id] = v
	}
	return out, nil
}

// retargetAllowed is whether a kept report may be repaired to t: one of its targets' channels (any
// thread of it), the home channel, or an owner's DM (Options.Owners).
func (s *Scheduler) retargetAllowed(p *pendingDelivery, t Target) bool {
	if t.Channel == "" {
		return true // the home channel
	}
	audience := p.Audience
	if len(audience) == 0 {
		for _, o := range p.Targets {
			audience = append(audience, o.Target)
		}
	}
	for _, o := range audience {
		if o.Platform == t.Platform && o.Channel == t.Channel {
			return true
		}
	}
	return s.o.Owners != nil && slices.Contains(s.o.Owners(), t.Channel) && t.Thread == ""
}

// syncAlertLater brings the home notice in line after a change made by a tool call (a discard):
// in the background, never holding the tool.
func (s *Scheduler) syncAlertLater() {
	go func() {
		if !s.deliverySendMu.TryLock() {
			return // a delivery under way syncs it when it ends
		}
		defer s.deliverySendMu.Unlock()
		s.deliveryMu.Lock()
		defer s.deliveryMu.Unlock()
		f, e := s.readDeliveries()
		if e != nil {
			return
		}
		ctx, cancel := context.WithTimeout(s.runCtx, 30*time.Second)
		defer cancel()
		s.syncAlert(ctx, f)
	}()
}

func (s *Scheduler) clearDeliveryHold(id string) bool {
	if e := s.mutate(func(f *File) (bool, error) {
		j := f.Find(id)
		if j == nil {
			return false, nil
		}
		j.Set("delivery_hold", false)
		j.Set("last_delivery_error", nil)
		if j.Str("last_status") == "delivery_failed" {
			j.Set("last_status", orDefault(j.Str("last_execution_status"), "ok"))
		}
		return true, nil
	}); e != nil {
		s.logf("could not clear delivery hold for job %s", id)
		return false
	}
	return true
}

// ReportHash is a kept report's text as an approval card binds it.
func ReportHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// KeptReport is the report kept for job id's delivery (its text and targets), for an approval
// card of its retry or discard.
func (s *Scheduler) KeptReport(id string) (string, []string, bool) {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	f, err := s.readDeliveries()
	if err != nil {
		return "", nil, false
	}
	p := f.Pending[id]
	if p == nil {
		return "", nil, false
	}
	var targets []string
	for _, t := range p.Targets {
		if !t.Sent {
			targets = append(targets, t.Target.String())
		}
	}
	return p.Text, targets, true
}
