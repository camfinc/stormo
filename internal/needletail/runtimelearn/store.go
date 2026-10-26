// Package runtimelearn collects shadow outcomes and optional review-only reflections.
// No transcript or archive is exported by outcome collection.
package runtimelearn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/pkg/learning"
)

const OutcomesFile = "state/needletail/outcomes-v1.json"
const OutcomesExportFile = "state/needletail/outcomes-export-v1.json"
const ReflectionsFile = "state/needletail/reflections-v1.json"
const ProposalsFile = "state/needletail/reflection-proposals-v1.json"

func Hash(text string) string { h := sha256.Sum256([]byte(text)); return hex.EncodeToString(h[:]) }

// Store owns version 1 runtime files, private 0600. Unknown versions fail closed and
// are never rewritten. Capacity exhaustion stops collection rather than erasing evidence.
type Store struct {
	Home    string
	Trusted func(string, func() error) error
	mu      sync.Mutex
}

func (s *Store) load() (*learning.EvidenceStore, error) {
	b, err := os.ReadFile(filepath.Join(s.Home, OutcomesFile))
	if os.IsNotExist(err) {
		return &learning.EvidenceStore{Version: 1, Events: []learning.OutcomeEvent{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var v learning.EvidenceStore
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if v.Version != 1 || len(v.Events) > learning.MaxOutcomeEvents {
		return nil, fmt.Errorf("unsupported or oversized outcomes store")
	}
	for _, e := range v.Events {
		if err = e.Validate(); err != nil {
			return nil, err
		}
	}
	return &v, nil
}
func (s *Store) write(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return fsx.WriteAtomicBelow(s.Home, path, filepath.Join(s.Home, fsx.TmpDir), b, 0600)
}
func (s *Store) Append(e learning.OutcomeEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(e)
}
func (s *Store) appendLocked(e learning.OutcomeEvent) error {
	v, err := s.load()
	if err != nil {
		return err
	}
	added, err := v.Ingest(e)
	if err != nil {
		return err
	}
	if added {
		if err = s.write(OutcomesFile, v); err != nil {
			return err
		}
	}
	exported := learning.EvidenceStore{Version: 1, Events: []learning.OutcomeEvent{}}
	for _, item := range v.Events {
		if item.Audience == "agent" {
			item.EvidenceRef = ""
			exported.Events = append(exported.Events, item)
		}
	}
	return s.write(OutcomesExportFile, exported)
}
func (s *Store) Events(root, audience string) ([]learning.OutcomeEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.load()
	if err != nil {
		return nil, err
	}
	out := []learning.OutcomeEvent{}
	for _, e := range v.Events {
		if e.TaskRoot == root && e.Audience == audience {
			out = append(out, e)
		}
	}
	return out, nil
}

// Feedback can refer only to an already observed subject revision. It is a host API,
// never a model tool. Origin is an opaque check/feedback identifier, not source text.
func (s *Store) Feedback(root, audience, eventID, outcome, origin string) (learning.OutcomeEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.load()
	if err != nil {
		return learning.OutcomeEvent{}, err
	}
	events := []learning.OutcomeEvent{}
	for _, e := range v.Events {
		if e.TaskRoot == root && e.Audience == audience {
			events = append(events, e)
		}
	}
	if outcome != "succeeded" && outcome != "failed" && outcome != "corrected" {
		return learning.OutcomeEvent{}, fmt.Errorf("invalid feedback outcome")
	}
	if origin == "" || len(origin) > 128 {
		return learning.OutcomeEvent{}, fmt.Errorf("opaque origin required")
	}
	for _, r := range origin {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return learning.OutcomeEvent{}, fmt.Errorf("origin must be opaque")
		}
	}
	for _, e := range events {
		if e.EventID == eventID && (e.SourceType == "runtime" || e.SourceType == "view") {
			e.EventID = Hash(root + "\n" + eventID + "\n" + origin + "\n" + outcome)
			e.SourceType = "authorized_feedback"
			e.Origin = Hash(origin)
			e.TaskOutcome = outcome
			e.At = time.Now().UTC().Format(time.RFC3339Nano)
			// Timestamp is stable on duplicate delivery, so immutable ingestion remains idempotent.
			for _, old := range events {
				if old.EventID == e.EventID {
					return old, s.appendLocked(old)
				}
			}
			return e, s.appendLocked(e)
		}
	}
	return learning.OutcomeEvent{}, fmt.Errorf("observed event not found in this audience")
}
