package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// EvidenceVersion owns a separate, private store; ledger and usage formats stay unchanged.
const EvidenceVersion = 1
const MaxOutcomeEvents = 4096
const MaxEvidenceBytes = MaxOutcomeEvents * 4096

// OutcomeEvent records attribution, not activity. Only assertion and authorized_feedback
// sources may confirm a task result. Origin identifies the underlying check or feedback,
// so replaying the same source under a different task cannot manufacture corroboration.
type OutcomeEvent struct {
	EventID         string `json:"eventId"`
	TaskRoot        string `json:"taskRoot"`
	SubjectKind     string `json:"subjectKind"`
	SubjectID       string `json:"subjectId"`
	SubjectRevision string `json:"subjectRevision"`
	At              string `json:"at"`
	Audience        string `json:"audience"`
	SourceType      string `json:"sourceType"`
	Origin          string `json:"origin"`
	ExecutionStatus string `json:"executionStatus"`
	TaskOutcome     string `json:"taskOutcome"`
	EvidenceRef     string `json:"evidenceRef,omitempty"`
}

type EvidenceStore struct {
	Version int            `json:"version"`
	Events  []OutcomeEvent `json:"events"`
}

// EvidenceSummary deliberately contains no source text, references, task IDs or audience data.
type EvidenceSummary struct {
	Revision      string   `json:"revision"`
	Unknown       bool     `json:"unknown"`
	Attempts      int      `json:"attempts"`
	Confirmations int      `json:"confirmations"`
	Failures      int      `json:"failures"`
	Corrections   int      `json:"corrections"`
	EvidenceTypes []string `json:"evidenceTypes"`
	ValidatedAt   string   `json:"validatedAt,omitempty"`
}

func EvidencePath(root, agent string) string {
	return filepath.Join(filepath.Dir(LedgerPath(root, agent)), "evidence-v1.json")
}
func LoadEvidence(root, agent string) (*EvidenceStore, error) {
	f, err := os.Open(EvidencePath(root, agent))
	if os.IsNotExist(err) {
		return &EvidenceStore{Version: EvidenceVersion, Events: []OutcomeEvent{}}, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, MaxEvidenceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxEvidenceBytes {
		return nil, fmt.Errorf("evidence byte limit exceeded")
	}
	var s EvidenceStore
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.Version != EvidenceVersion {
		return nil, fmt.Errorf("unsupported evidence version %d", s.Version)
	}
	if len(s.Events) > MaxOutcomeEvents {
		return nil, fmt.Errorf("evidence event limit exceeded")
	}
	for _, e := range s.Events {
		if err = e.Validate(); err != nil {
			return nil, err
		}
	}
	return &s, nil
}

// ReadEvidence is the store as its readers see it (the ledger, the compile, the review): a
// store they cannot read (corrupt, oversized, invalid, a version this build does not know, or
// unreadable) reads as no evidence, and the reason comes back as a warning, logged once per
// store and reason. A missing store is empty and no warning. Only MergeEvidence, which writes,
// refuses an unreadable store (so it is never overwritten).
func ReadEvidence(root, agent string) (*EvidenceStore, string) {
	s, err := LoadEvidence(root, agent)
	if err == nil {
		return s, ""
	}
	warning := "outcome evidence (" + filepath.Base(EvidencePath(root, agent)) + ") is unreadable, so it reads as none: " + err.Error()
	if _, seen := evidenceWarned.LoadOrStore(EvidencePath(root, agent)+"\n"+warning, true); !seen {
		log.Printf("learning: %s: %s", agent, warning)
	}
	return &EvidenceStore{Version: EvidenceVersion, Events: []OutcomeEvent{}}, warning
}

// evidenceWarned are the warnings ReadEvidence logged (store path and reason).
var evidenceWarned sync.Map

func (e OutcomeEvent) Validate() error {
	for _, s := range []string{e.EventID, e.TaskRoot, e.SubjectKind, e.SubjectID, e.SubjectRevision, e.Audience, e.SourceType, e.Origin} {
		if len(s) == 0 || len(s) > 256 {
			return fmt.Errorf("evidence identity missing or too long")
		}
	}
	if len(e.At) > 64 {
		return fmt.Errorf("evidence timestamp too long")
	}
	if len(e.EvidenceRef) > 512 {
		return fmt.Errorf("evidence reference too long")
	}
	if _, err := time.Parse(time.RFC3339Nano, e.At); err != nil {
		return fmt.Errorf("evidence timestamp: %w", err)
	}
	switch e.SourceType {
	case "assertion", "authorized_feedback", "model", "runtime", "view", "snapshot":
	default:
		return fmt.Errorf("unknown evidence source")
	}
	switch e.TaskOutcome {
	case "succeeded", "failed", "corrected", "unknown":
	default:
		return fmt.Errorf("unknown task outcome")
	}
	switch e.ExecutionStatus {
	case "succeeded", "failed", "unknown":
	default:
		return fmt.Errorf("unknown execution status")
	}
	return nil
}

// Ingest is immutable and idempotent. A reused event ID with different data is rejected.
func (s *EvidenceStore) Ingest(e OutcomeEvent) (bool, error) {
	if s.Version != EvidenceVersion {
		return false, fmt.Errorf("unsupported evidence version")
	}
	if err := e.Validate(); err != nil {
		return false, err
	}
	for _, old := range s.Events {
		if old.EventID == e.EventID {
			if old != e {
				return false, fmt.Errorf("conflicting evidence event ID")
			}
			return false, nil
		}
	}
	if len(s.Events) >= MaxOutcomeEvents {
		return false, fmt.Errorf("evidence store full; archive before collecting more")
	}
	s.Events = append(s.Events, e)
	return true, nil
}
func SaveEvidence(root, agent string, s *EvidenceStore) error {
	if s.Version != EvidenceVersion || len(s.Events) > MaxOutcomeEvents {
		return fmt.Errorf("invalid evidence store")
	}
	for _, e := range s.Events {
		if err := e.Validate(); err != nil {
			return err
		}
	}
	p := EvidencePath(root, agent)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".evidence-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
func LessonRevision(e *Learning) string { return digest(string(e.Kind) + "\n" + e.Text) }
func digest(s string) string            { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// Summary requires exact audience and revision matches. Neither views, snapshots, model
// claims nor successful command exits imply successful task assertions.
func (s *EvidenceStore) Summary(kind, id, revision, audience string) EvidenceSummary {
	out := EvidenceSummary{Unknown: true, EvidenceTypes: []string{}}
	tasks, origins, types := map[string]bool{}, map[string]bool{}, map[string]bool{}
	ids := []string{}
	negatives := map[string]bool{}
	events := append([]OutcomeEvent{}, s.Events...)
	sort.Slice(events, func(i, j int) bool { return events[i].EventID < events[j].EventID })
	for _, e := range events {
		if e.SubjectKind != kind || e.SubjectID != id || e.SubjectRevision != revision || e.Audience != audience {
			continue
		}
		ids = append(ids, digest(fmt.Sprintf("%+v", e)))
		types[e.SourceType] = true
		if e.SourceType == "snapshot" || e.SourceType == "view" {
			continue
		}
		tasks[e.TaskRoot] = true
		strong := e.SourceType == "assertion" || e.SourceType == "authorized_feedback"
		if strong && e.TaskOutcome != "unknown" || e.SourceType == "runtime" && e.TaskOutcome == "failed" {
			out.Unknown = false
		}
		if e.TaskOutcome == "failed" && !negatives["failed:"+e.Origin] {
			negatives["failed:"+e.Origin] = true
			out.Failures++
		}
		if e.TaskOutcome == "corrected" && !negatives["corrected:"+e.Origin] {
			negatives["corrected:"+e.Origin] = true
			out.Corrections++
		}
		if strong && e.TaskOutcome == "succeeded" && !origins[e.Origin] && !origins["task:"+e.TaskRoot] {
			out.Confirmations++
			origins[e.Origin] = true
			origins["task:"+e.TaskRoot] = true
			at, _ := time.Parse(time.RFC3339Nano, e.At)
			previous, _ := time.Parse(time.RFC3339Nano, out.ValidatedAt)
			if at.After(previous) {
				out.ValidatedAt = at.UTC().Format("2006-01-02T15:04:05.000000000Z")
			}
		}
	}
	out.Attempts = len(tasks)
	for typ := range types {
		out.EvidenceTypes = append(out.EvidenceTypes, typ)
	}
	sort.Strings(out.EvidenceTypes)
	sort.Strings(ids)
	b, _ := json.Marshal(ids)
	out.Revision = digest(string(b))
	return out
}
func (s EvidenceSummary) AutoEligible(min int) bool {
	if min < 1 {
		min = 1
	}
	return s.Confirmations >= min && s.Failures == 0 && s.Corrections == 0
}

// MergeEvidence serializes read-modify-write across processes. A concurrent writer gets
// an explicit busy error and may replay its batch; the watermark must not advance on error.
func MergeEvidence(root, agent string, events []OutcomeEvent) error {
	p := EvidencePath(root, agent)
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	lockPath := filepath.Join(root, ".swarm", "learning-locks", digest(agent)+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("evidence writer lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("evidence writer busy: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	s, err := LoadEvidence(root, agent)
	if err != nil {
		return err
	}
	for _, e := range events {
		if _, err = s.Ingest(e); err != nil {
			return err
		}
	}
	return SaveEvidence(root, agent, s)
}

// ExportableEvidence is the narrow runtime-to-review boundary. All provenance is
// opaque and only deliberately agent-scoped outcomes leave the private store.
func ExportableEvidence(e OutcomeEvent) bool {
	if e.Validate() != nil || e.Audience != "agent" || e.EvidenceRef != "" {
		return false
	}
	if e.SubjectKind != "lesson" && e.SubjectKind != "skill" && e.SubjectKind != "memory" {
		return false
	}
	for _, id := range []string{e.EventID, e.TaskRoot, e.Origin, e.SubjectRevision} {
		if !hexIdentity(id, 64) {
			return false
		}
	}
	return hexIdentity(e.SubjectID, 64) || e.SubjectKind == "lesson" && hexIdentity(e.SubjectID, 12)
}
func hexIdentity(s string, n int) bool {
	if len(s) != n {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
