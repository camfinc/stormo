package runtimelearn

import (
	"fmt"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/learning"
)

// DeliveryObservation is trusted scheduler metadata, never message text, provider
// errors or targets. A stable report ID names one failed/recovered outbox report.
type DeliveryObservation struct {
	ID, JobID, TaskRoot string
	At                  time.Time
	Status              string
	Failure             bool // the original workload failed; delivery recovery does not fix it
}

// RecordDelivery retains transport failure/recovery in the private outcome store.
// It never credits knowledge or invokes reflection. Replayed observations preserve
// their first timestamp, while conflicting reuse of an ID is rejected.
func (s *Store) RecordDelivery(in DeliveryObservation) error {
	if in.Status != "failed" && in.Status != "recovered" {
		return fmt.Errorf("invalid delivery observation status")
	}
	suffix := ":" + in.Status
	if !strings.HasSuffix(in.ID, suffix) || len(in.ID) <= len(suffix) || len(in.ID) > 256 || in.JobID == "" || len(in.JobID) > 256 || len(in.TaskRoot) > 512 || in.At.IsZero() {
		return fmt.Errorf("invalid delivery observation identity")
	}
	base := strings.TrimSuffix(in.ID, suffix)
	root := in.TaskRoot
	if root == "" {
		root = "scheduled-delivery:" + base
	}
	execution := "failed"
	if in.Status == "recovered" {
		execution = "succeeded"
	}
	original := "not-failed"
	if in.Failure {
		original = "failed"
	}
	event := learning.OutcomeEvent{
		EventID: Hash("scheduled-delivery-event\n" + in.ID), TaskRoot: Hash(root),
		SubjectKind: "delivery", SubjectID: Hash("scheduled-delivery-report\n" + base),
		SubjectRevision: Hash("scheduled-delivery-v1\n" + in.JobID + "\n" + original),
		At:              in.At.UTC().Format(time.RFC3339Nano), Audience: "conversation:" + Hash(root),
		SourceType: "runtime", Origin: Hash("scheduled-delivery-report\n" + base),
		ExecutionStatus: execution, TaskOutcome: "unknown",
		EvidenceRef: "scheduled-delivery:" + in.Status + ";original-execution:" + original,
	}
	if in.Status == "failed" {
		event.TaskOutcome = "failed"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, err := s.load()
	if err != nil {
		return err
	}
	for _, old := range previous.Events {
		if old.EventID == event.EventID {
			event.At = old.At
			if old != event {
				return fmt.Errorf("conflicting delivery observation identity")
			}
			return s.appendLocked(old)
		}
	}
	return s.appendLocked(event)
}
