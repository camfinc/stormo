package sched

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// DeliveryAlerter sends only a constant runtime notice to the configured home.
// It must not forward report content or run user/model handlers.
type DeliveryAlerter interface{ AlertDelivery(context.Context) error }

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
			if e != nil {
				last = e
				var permanent interface{ DeliveryPermanent() bool }
				if errors.As(e, &permanent) && permanent.DeliveryPermanent() {
					p.Blocked = true
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
	if p.complete() {
		if e := s.writeDeliveries(f); e != nil {
			return "Could not persist completed delivery."
		}
		if p.Failed && !p.Quarantined && s.o.OnDelivery != nil {
			s.o.OnDelivery(DeliveryEvent{ID: p.ID + ":recovered", JobID: p.JobID, Session: p.Session, At: s.o.Now(), Status: "recovered", Failure: p.Failure})
		}
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
	s.alertDelivery(ctx, f, p)
	if p.Quarantined {
		return "scheduled forwarding was refused because this run is quarantined."
	}
	if last != nil {
		s.logf("job %s: delivery attempt failed: %s", p.JobID, s.o.Env.Redact(last.Error()))
	}
	return "Report retained pending delivery; review destination and retry_delivery."
}
func (s *Scheduler) alertDelivery(ctx context.Context, f *deliveryFile, p *pendingDelivery) {
	if p.Alerted || p.AlertAttempts >= 3 {
		return
	}
	a, ok := s.o.Deliver.(DeliveryAlerter)
	if !ok {
		return
	}
	p.AlertAttempts++
	if s.writeDeliveries(f) != nil {
		return
	}
	s.deliveryMu.Unlock()
	e := a.AlertDelivery(ctx)
	s.deliveryMu.Lock()
	if e == nil {
		p.Alerted = true
	} else {
		s.logf("scheduled delivery home alert failed")
	}
	if e := s.writeDeliveries(f); e != nil {
		s.logf("could not persist delivery alert status")
	}
}

// recoverDeliveries never executes the model or re-analyzes the source material.
func (s *Scheduler) recoverDeliveries() {
	s.deliverySendMu.Lock()
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
			s.alertDelivery(ctx, f, p)
			p.Next = s.o.Now().Add(30 * time.Minute)
			_ = s.writeDeliveries(f)
		}
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
	if a.Action == "discard_delivery" {
		delete(f.Pending, a.JobID)
		return map[string]any{"discarded": true}, s.writeDeliveries(f)
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
		out[id] = map[string]any{"pending": true, "attempts": p.Attempts, "needs_review": p.Blocked, "home_alert_sent": p.Alerted, "quarantined": p.Quarantined}
	}
	return out, nil
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
