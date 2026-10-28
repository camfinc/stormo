package runtimelearn

import (
	"encoding/json"
	"fmt"
	"time"
)

// BackgroundJob is a scoped owner view. Source sessions and summaries stay private.
type BackgroundJob struct {
	CreatedAt    time.Time   `json:"createdAt"`
	UpdatedAt    time.Time   `json:"updatedAt"`
	LastCode     string      `json:"lastCode,omitempty"`
	ID           string      `json:"id"`
	State        string      `json:"state"`
	Kind         TriggerKind `json:"kind"`
	Attempts     int         `json:"attempts"`
	OwnerRetries int         `json:"ownerRetries"`
	Generation   int         `json:"generation"`
	Next         time.Time   `json:"next"`
	Updated      time.Time   `json:"updated"`
	ResultCode   string      `json:"resultCode,omitempty"`
	CandidateID  string      `json:"candidateId,omitempty"`
}
type backgroundTombstone struct {
	Kind         TriggerKind `json:"kind,omitempty"`
	EvidenceRefs []string    `json:"evidenceRefs,omitempty"`
	Candidate    *Candidate  `json:"candidate,omitempty"`
	ID           string      `json:"id"`
	Root         string      `json:"root"`
	Audience     string      `json:"audience"`
	State        string      `json:"state"`
	Attempts     int         `json:"attempts"`
}

func backgroundView(j backgroundJob) BackgroundJob {
	out := BackgroundJob{CreatedAt: j.Created, UpdatedAt: j.Updated, LastCode: j.ResultCode, ID: j.ID, State: j.State, Kind: j.Trigger.Kind, Attempts: j.Attempts, OwnerRetries: j.OwnerRetries, Generation: j.Generation, Next: j.Next, Updated: j.Updated, ResultCode: j.ResultCode}
	if j.Candidate != nil {
		out.CandidateID = j.Candidate.ID
	}
	return out
}
func (s *Store) BackgroundJobs(root, audience string, now time.Time) ([]BackgroundJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.background()
	if err != nil {
		return nil, err
	}
	out := []BackgroundJob{}
	for _, j := range v.Jobs {
		if j.Trigger.Root == root && j.Trigger.Audience == audience {
			out = append(out, backgroundView(j))
		}
	}
	return out, nil
}

// CancelBackground invalidates the durable generation before an in-flight model
// can persist its result. The paid reservation is deliberately retained.
func (s *Store) CancelBackground(root, audience, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.background()
	if err != nil {
		return err
	}
	for i := range v.Jobs {
		j := &v.Jobs[i]
		if j.ID != id || j.Trigger.Root != root || j.Trigger.Audience != audience {
			continue
		}
		if j.State == "cancelled" {
			return nil
		}
		if j.State != "pending" && j.State != "reserved" {
			return fmt.Errorf("background job is terminal")
		}
		j.Generation++
		j.Updated = time.Now().UTC()
		j.State = "cancelled"
		j.ResultCode = "owner_cancelled"
		j.Candidate = nil
		return s.writeBackground(v)
	}
	for _, t := range v.Tombstones {
		if t.ID == id && t.Root == root && t.Audience == audience && t.State == "cancelled" {
			return nil
		}
	}
	return fmt.Errorf("background job not found")
}

// RetryBackground is an explicit owner decision, including for uncertain calls.
// It does not clear any paid reservation: processing reserves again under the
// current rolling budget. Total attempts remain bounded to three across restarts.
func (s *Store) RetryBackground(root, audience, id string, now time.Time) error {
	// Resolve source without holding the trust guard in the opposite lock order.
	s.mu.Lock()
	v, err := s.background()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	session := ""
	for _, j := range v.Jobs {
		if j.ID == id && j.Trigger.Root == root && j.Trigger.Audience == audience {
			session = j.Trigger.Session
		}
	}
	s.mu.Unlock()
	if session == "" {
		return fmt.Errorf("background job not found")
	}
	return s.backgroundTrusted(session, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		v, err := s.background()
		if err != nil {
			return err
		}
		for i := range v.Jobs {
			j := &v.Jobs[i]
			if j.ID != id || j.Trigger.Root != root || j.Trigger.Audience != audience {
				continue
			}
			if j.State != "failed" && j.State != "interrupted" && j.State != "cancelled" {
				return fmt.Errorf("background job cannot retry")
			}
			if j.Attempts >= 3 || j.OwnerRetries >= 2 {
				return fmt.Errorf("background retry limit reached")
			}
			if err := s.eligibleBackground(j.Trigger); err != nil {
				return err
			}
			j.OwnerRetries++
			j.Generation++
			j.State = "pending"
			j.Next = now.UTC()
			j.Updated = now.UTC()
			j.ResultCode = "owner_retry"
			j.Candidate = nil
			return s.writeBackground(v)
		}
		return fmt.Errorf("background job not found")
	})
}

// PruneBackground compacts only terminal details older than thirty days in the
// authorized scope. Tombstones keep replay identities; reservations are untouched.
// Uncertain jobs remain reviewable and are never compacted automatically.
func (s *Store) PruneBackground(root, audience string, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.background()
	if err != nil {
		return 0, err
	}
	kept := make([]backgroundJob, 0, len(v.Jobs))
	n := 0
	for _, j := range v.Jobs {
		updated := j.Updated
		if updated.IsZero() {
			updated = j.Next
		}
		terminal := j.State == "complete" || j.State == "failed" || j.State == "cancelled"
		if j.Trigger.Root == root && j.Trigger.Audience == audience && terminal && !updated.After(now.Add(-30*24*time.Hour)) {
			if len(v.Tombstones) >= backgroundTombstoneCapacity {
				return 0, fmt.Errorf("background tombstone capacity exhausted")
			}
			v.Tombstones = append(v.Tombstones, backgroundTombstone{ID: j.ID, Root: root, Audience: audience, State: j.State, Attempts: j.Attempts, Kind: j.Trigger.Kind, EvidenceRefs: append([]string(nil), j.Trigger.EvidenceRefs...), Candidate: j.Candidate})
			n++
		} else {
			kept = append(kept, j)
		}
	}
	if n == 0 {
		return 0, nil
	}
	v.Jobs = kept
	if err := s.writeBackground(v); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) writeBackground(v *backgroundState) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(v.Jobs) > backgroundCapacity || len(v.Tombstones) > backgroundTombstoneCapacity || len(v.Reservations) > 32 || len(b) > backgroundBytes {
		return fmt.Errorf("background state byte capacity exhausted")
	}
	return s.write(BackgroundFile, json.RawMessage(b))
}
