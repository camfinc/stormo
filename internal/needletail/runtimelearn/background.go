package runtimelearn

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/llm"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackgroundFile is private runtime state, separate from the legacy explicit
// reflection store and its unchanged 100-call lifetime ceiling.
const BackgroundFile = "state/needletail/background-learning-v1.json"
const backgroundCapacity = 256
const backgroundTombstoneCapacity = 4096
const backgroundBytes = 16 << 20
const BackgroundBackupFile = "state/needletail/background-learning-v1.backup.json"

var ErrBackgroundBudget = errors.New("background daily budget exhausted")

type TriggerKind string

const (
	TriggerTaskCompleted     TriggerKind = "task_completed"
	TriggerToolFailure       TriggerKind = "tool_failure"
	TriggerFeedbackCorrected TriggerKind = "feedback_corrected"
	TriggerRecovery          TriggerKind = "recovery"
)

// BackgroundTrigger contains bounded host-selected evidence, never a transcript.
// Session is retained privately to recheck the source conversation's quarantine.
type BackgroundTrigger struct {
	Kind         TriggerKind `json:"kind"`
	Session      string      `json:"session"`
	Root         string      `json:"root"`
	Audience     string      `json:"audience"`
	Summary      string      `json:"summary"`
	EvidenceRefs []string    `json:"evidenceRefs"`
}
type BackgroundConfig struct{ DailyCalls, DailyTokens, MaxAttempts int }

func (c BackgroundConfig) limits() BackgroundConfig {
	if c.DailyCalls <= 0 || c.DailyCalls > 32 {
		c.DailyCalls = 8
	}
	if c.DailyTokens <= 0 || c.DailyTokens > 32768 {
		c.DailyTokens = 4096
	}
	if c.MaxAttempts <= 0 || c.MaxAttempts > 3 {
		c.MaxAttempts = 2
	}
	return c
}

// BackgroundReflector must enforce a 512 output-token ceiling and no tools.
// Every invocation, including errors, consumes its pre-persisted reservation.
// BackgroundRetryableError permits a bounded retry only when the adapter knows
// no ambiguous provider execution occurred. Ordinary provider errors are terminal.
type BackgroundRetryableError struct{ Err error }

func (e *BackgroundRetryableError) Error() string { return e.Err.Error() }
func (e *BackgroundRetryableError) Unwrap() error { return e.Err }

type BackgroundReflector func(context.Context, BackgroundTrigger) (*Candidate, error)
type backgroundJob struct {
	Created      time.Time         `json:"created"`
	ID           string            `json:"id"`
	Trigger      BackgroundTrigger `json:"trigger"`
	State        string            `json:"state"`
	Attempts     int               `json:"attempts"`
	Next         time.Time         `json:"next"`
	Updated      time.Time         `json:"updated"`
	Generation   int               `json:"generation"`
	OwnerRetries int               `json:"ownerRetries"`
	ResultCode   string            `json:"resultCode,omitempty"`
	Candidate    *Candidate        `json:"candidate,omitempty"`
}
type backgroundReservation struct {
	At     time.Time `json:"at"`
	Tokens int       `json:"tokens"`
}
type backgroundState struct {
	Version      int                     `json:"version"`
	Jobs         []backgroundJob         `json:"jobs"`
	Reservations []backgroundReservation `json:"reservations"`
	Tombstones   []backgroundTombstone   `json:"tombstones,omitempty"`
}
type BackgroundStatus struct{ Pending, Completed, Failed, Interrupted, CallsUsed, TokensReserved int }

func (s *Store) background() (*backgroundState, error) {
	b, err := fsx.ReadFileBelow(s.Home, BackgroundFile, backgroundBytes)
	if os.IsNotExist(err) {
		return &backgroundState{Version: 2, Jobs: []backgroundJob{}, Reservations: []backgroundReservation{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var v backgroundState
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if (v.Version != 1 && v.Version != 2) || (v.Version == 1 && len(b) > 4<<20) || len(v.Jobs) > backgroundCapacity || len(v.Reservations) > 32 || len(v.Tombstones) > backgroundTombstoneCapacity {
		return nil, fmt.Errorf("unsupported background state")
	}
	seen := map[string]bool{}
	for _, j := range v.Jobs {
		if seen[j.ID] || j.ID != backgroundIdentity(j.Trigger) || validateTrigger(j.Trigger) != nil || j.Attempts < 0 || j.Attempts > 3 || j.OwnerRetries < 0 || j.OwnerRetries > 2 || j.Generation < 0 || j.Next.IsZero() {
			return nil, fmt.Errorf("invalid background job")
		}
		if j.Candidate != nil {
			c := j.Candidate
			if j.State != "complete" || validateBackgroundCandidate(c) != nil || c.Root != j.Trigger.Root || c.Audience != j.Trigger.Audience || c.ID != Hash(j.ID+"\n"+c.Text) || strings.Join(c.EventRefs, "\n") != strings.Join(j.Trigger.EvidenceRefs, "\n") {
				return nil, fmt.Errorf("invalid stored background candidate")
			}
		}
		seen[j.ID] = true
		switch j.State {
		case "pending", "reserved", "complete", "failed", "interrupted", "cancelled":
		default:
			return nil, fmt.Errorf("invalid background job state")
		}
	}
	for _, r := range v.Reservations {
		if r.At.IsZero() || r.Tokens != 512 {
			return nil, fmt.Errorf("invalid reservation")
		}
	}
	for _, t := range v.Tombstones {
		if !backgroundHash(t.ID) || !backgroundHash(t.Root) || t.Audience == "" || len(t.Audience) > 256 || seen[t.ID] || t.Attempts < 0 || t.Attempts > 3 || (t.State != "complete" && t.State != "failed" && t.State != "cancelled") {
			return nil, fmt.Errorf("invalid background tombstone")
		}
		if c := t.Candidate; c != nil {
			if t.State != "complete" || validateBackgroundCandidate(c) != nil || c.Root != t.Root || c.Audience != t.Audience || c.ID != Hash(t.ID+"\n"+c.Text) || validateTrigger(BackgroundTrigger{Session: "retained", Kind: t.Kind, Root: t.Root, Audience: t.Audience, EvidenceRefs: t.EvidenceRefs}) != nil || strings.Join(c.EventRefs, "\n") != strings.Join(t.EvidenceRefs, "\n") || backgroundIdentity(BackgroundTrigger{Kind: t.Kind, Root: t.Root, Audience: t.Audience, EvidenceRefs: t.EvidenceRefs}) != t.ID {
				return nil, fmt.Errorf("invalid retained background candidate")
			}
			for _, ref := range t.EvidenceRefs {
				if !backgroundHash(ref) {
					return nil, fmt.Errorf("invalid retained evidence reference")
				}
			}
		}
		seen[t.ID] = true
	}
	if v.Version == 1 {
		// Keep the previous exact document for explicit rollback. Updating the
		// existing filename atomically makes older runtimes fail closed on v2.
		if err := fsx.WriteAtomicBelow(s.Home, BackgroundBackupFile, filepath.Join(s.Home, fsx.TmpDir), b, 0600); err != nil {
			return nil, err
		}
		v.Version = 2
		for i := range v.Jobs {
			v.Jobs[i].Updated = v.Jobs[i].Next
			v.Jobs[i].Created = v.Jobs[i].Next
		}
		if err := s.writeBackground(&v); err != nil {
			return nil, err
		}
	}
	return &v, nil
}
func backgroundHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && s == strings.ToLower(s)
}
func backgroundIdentity(t BackgroundTrigger) string {
	refs := append([]string(nil), t.EvidenceRefs...)
	sort.Strings(refs)
	return Hash(string(t.Kind) + "\n" + t.Root + "\n" + t.Audience + "\n" + strings.Join(refs, "\n"))
}

func validateTrigger(t BackgroundTrigger) error {
	switch t.Kind {
	case TriggerTaskCompleted, TriggerToolFailure, TriggerFeedbackCorrected, TriggerRecovery:
	default:
		return fmt.Errorf("invalid background trigger")
	}
	if t.Session == "" || len(t.Session) > 256 || !backgroundHash(t.Root) || t.Audience == "" || len(t.Audience) > 256 || len(t.Summary) > 2000 || len(t.EvidenceRefs) == 0 || len(t.EvidenceRefs) > 8 {
		return fmt.Errorf("invalid background evidence")
	}
	for _, r := range t.EvidenceRefs {
		if !backgroundHash(r) {
			return fmt.Errorf("opaque evidence reference required")
		}
	}
	return nil
}

// eligibleBackground resolves every evidence reference against immutable observed
// outcomes. A caller-supplied trigger label cannot manufacture a successful check.
func (s *Store) eligibleBackground(t BackgroundTrigger) error {
	v, err := s.load()
	if err != nil {
		return err
	}
	eligible := false
	for _, ref := range t.EvidenceRefs {
		found := false
		for _, e := range v.Events {
			if e.EventID != ref || e.TaskRoot != t.Root || e.Audience != t.Audience {
				continue
			}
			found = true
			switch t.Kind {
			case TriggerToolFailure:
				eligible = eligible || e.ExecutionStatus == "failed"
			case TriggerFeedbackCorrected:
				eligible = eligible || (e.SourceType == "authorized_feedback" && e.TaskOutcome == "corrected")
			case TriggerRecovery, TriggerTaskCompleted:
				eligible = eligible || ((e.SourceType == "assertion" || e.SourceType == "authorized_feedback") && e.TaskOutcome == "succeeded")
			}
		}
		if !found {
			return fmt.Errorf("background evidence not found in source audience")
		}
	}
	if !eligible {
		return fmt.Errorf("background trigger lacks eligible evidence")
	}
	return nil
}

func (s *Store) backgroundTrusted(session string, f func() error) error {
	if s.Trusted == nil {
		return fmt.Errorf("background learning requires source trust guard")
	}
	return s.Trusted(session, f)
}
func (s *Store) EnqueueBackground(t BackgroundTrigger, now time.Time) (string, error) {
	if err := validateTrigger(t); err != nil {
		return "", err
	}
	t.EvidenceRefs = append([]string(nil), t.EvidenceRefs...)
	sort.Strings(t.EvidenceRefs)
	// Summary is deliberately excluded: redelivery cannot spend again by rewording it.
	id := backgroundIdentity(t)
	err := s.backgroundTrusted(t.Session, func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		v, err := s.background()
		if err != nil {
			return err
		}
		if err := s.eligibleBackground(t); err != nil {
			return err
		}
		for _, j := range v.Jobs {
			if j.ID == id {
				return nil
			}
		}
		for _, t := range v.Tombstones {
			if t.ID == id {
				return nil
			}
		}
		if len(v.Jobs) >= backgroundCapacity {
			return fmt.Errorf("background queue capacity exhausted")
		}
		v.Jobs = append(v.Jobs, backgroundJob{ID: id, Trigger: t, State: "pending", Next: now.UTC(), Updated: now.UTC(), Created: now.UTC()})
		return s.writeBackground(v)
	})
	return id, err
}

// RecoverBackground is called once at startup, before the worker starts. An
// uncertain in-flight provider call is terminal, never automatically replayed.
func (s *Store) RecoverBackground() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.background()
	if err != nil {
		return err
	}
	changed := false
	for i := range v.Jobs {
		if v.Jobs[i].State == "reserved" {
			v.Jobs[i].State = "interrupted"
			v.Jobs[i].ResultCode = "uncertain_call"
			v.Jobs[i].Generation++
			changed = true
		}
	}
	if changed {
		return s.writeBackground(v)
	}
	return nil
}

// ProcessBackground handles at most one job. Calls on a Store are safe concurrently;
// the runtime must remain the single writer of its home, as for outcome storage.
func (s *Store) ProcessBackground(ctx context.Context, now time.Time, cfg BackgroundConfig, reflect BackgroundReflector, check func(string) error) (bool, error) {
	return s.ProcessBackgroundWithResult(ctx, now, cfg, reflect, check, nil)
}

// ProcessBackgroundWithResult stages optional host-selected proposal output only
// after final source trust and reservation checks. finish must not call this Store.
func (s *Store) ProcessBackgroundWithResult(ctx context.Context, now time.Time, cfg BackgroundConfig, reflect BackgroundReflector, check func(string) error, finish func(BackgroundTrigger, *Candidate) error) (bool, error) {
	s.backgroundMu.Lock()
	defer s.backgroundMu.Unlock()
	if reflect == nil {
		return false, fmt.Errorf("background reflector unavailable")
	}
	cfg = cfg.limits()
	s.mu.Lock()
	v, err := s.background()
	if err != nil {
		s.mu.Unlock()
		return false, err
	}
	var selected *backgroundJob
	for _, j := range v.Jobs {
		if j.State == "pending" && !j.Next.After(now) {
			copy := j
			selected = &copy
			break
		}
	}
	s.mu.Unlock()
	if selected == nil {
		return false, nil
	}
	job := *selected
	if err := s.backgroundTrusted(job.Trigger.Session, func() error { return nil }); err != nil {
		if persistErr := s.interruptBackground(job.ID, job.Generation); persistErr != nil {
			return false, persistErr
		}
		return true, err
	}
	err = s.backgroundTrusted(job.Trigger.Session, func() error {
		if check != nil {
			body, _ := json.Marshal(job.Trigger)
			if err := check(string(body)); err != nil {
				return err
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		v, err := s.background()
		if err != nil {
			return err
		}
		if err := s.eligibleBackground(job.Trigger); err != nil {
			return err
		}
		kept := v.Reservations[:0]
		tokens := 0
		for _, r := range v.Reservations {
			if r.At.After(now.Add(-24 * time.Hour)) {
				kept = append(kept, r)
				tokens += r.Tokens
			}
		}
		v.Reservations = kept
		if len(kept) >= cfg.DailyCalls || tokens+512 > cfg.DailyTokens {
			return ErrBackgroundBudget
		}
		for i := range v.Jobs {
			j := &v.Jobs[i]
			if j.ID == job.ID {
				if j.State != "pending" || j.Next.After(now) || j.Generation != job.Generation {
					return fmt.Errorf("background job already reserved")
				}
				j.State = "reserved"
				j.Generation++
				j.Updated = now.UTC()
				j.Attempts++
				job = *j
				v.Reservations = append(v.Reservations, backgroundReservation{now.UTC(), 512})
				return s.writeBackground(v)
			}
		}
		return fmt.Errorf("background job disappeared")
	})
	if err != nil {
		if !errors.Is(err, ErrBackgroundBudget) {
			_ = s.interruptBackground(job.ID, job.Generation)
			return true, err
		}
		return false, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	candidate, callErr := reflect(callCtx, job.Trigger)
	cancel()
	if callErr == nil && candidate != nil {
		// Identity and scope are host-owned. Validate the final persisted shape,
		// since selected evidence and scope can add hundreds of bytes.
		candidate.ID = Hash(job.ID + "\n" + candidate.Text)
		candidate.Root = job.Trigger.Root
		candidate.Audience = job.Trigger.Audience
		candidate.EventRefs = append([]string(nil), job.Trigger.EvidenceRefs...)
		callErr = validateBackgroundCandidate(candidate)
	}
	err = s.backgroundTrusted(job.Trigger.Session, func() error {
		if candidate != nil && callErr == nil && check != nil {
			body, _ := json.Marshal(candidate)
			if err := check(string(body)); err != nil {
				return err
			}
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		v, err := s.background()
		if err != nil {
			return err
		}
		for i := range v.Jobs {
			j := &v.Jobs[i]
			if j.ID != job.ID {
				continue
			}
			if j.State != "reserved" || j.Generation != job.Generation {
				return fmt.Errorf("background reservation lost")
			}
			if candidate != nil && callErr == nil && finish != nil {
				if err := finish(job.Trigger, candidate); err != nil {
					return err
				}
			}
			j.Updated = now.UTC()
			if callErr != nil {
				j.State = "failed"
				j.ResultCode = "model_failed"
				var retryable *BackgroundRetryableError
				if errors.As(callErr, &retryable) && j.Attempts < cfg.MaxAttempts && ctx.Err() == nil {
					j.State = "pending"
					j.Next = now.Add(time.Duration(j.Attempts) * time.Minute)
				}
			} else {
				j.State = "complete"
				j.ResultCode = "completed"
				if candidate != nil {
					j.Candidate = candidate
				}
			}
			return s.writeBackground(v)
		}
		return fmt.Errorf("background job disappeared")
	})
	if err != nil {
		_ = s.interruptBackground(job.ID, job.Generation)
		return true, err
	}
	return true, callErr
}

// Terminal bookkeeping may proceed after quarantine; it contains no generated text.
func (s *Store) interruptBackground(id string, generation int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.background()
	if err != nil {
		return err
	}
	for i := range v.Jobs {
		if v.Jobs[i].ID == id {
			if v.Jobs[i].State == "cancelled" || v.Jobs[i].Generation != generation {
				return nil
			}
			v.Jobs[i].ResultCode = "uncertain_call"
			v.Jobs[i].State = "interrupted"
			v.Jobs[i].Candidate = nil
			return s.writeBackground(v)
		}
	}
	return fmt.Errorf("background job disappeared")
}

func validateBackgroundCandidate(c *Candidate) error {
	if d := c.SkillDraft; d != nil {
		if d.Name == "" || len(d.Name) > 256 || path.Clean(d.Name) != d.Name || path.IsAbs(d.Name) || (d.Name == "." || d.Name == "..") || strings.HasPrefix(d.Name, "../") || strings.ContainsAny(d.Name, "\\\x00\r\n") || !backgroundHash(d.ExpectedRevision) || d.OldText == "" || len(d.OldText) > 1000 || len(d.NewText) > 1000 || d.OldText == d.NewText {
			return fmt.Errorf("invalid bounded skill draft")
		}
	}
	b, _ := json.Marshal(c)
	if len(b) > 4000 || c.Text == "" || len(c.Text) > 1000 || c.Checks == "" || c.Conditions == "" || c.Sensitivity == "" || len(c.RelatedIDs) > 0 {
		return fmt.Errorf("invalid background candidate")
	}
	if c.Action != "new" {
		return fmt.Errorf("background candidate must propose new review material")
	}
	_, err := time.Parse("2006-01-02", c.ReviewDate)
	return err
}
func (s *Store) BackgroundSnapshot(root, audience string, now time.Time) (BackgroundStatus, []Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.background()
	out := BackgroundStatus{}
	c := []Candidate{}
	if err != nil {
		return out, c, err
	}
	for _, j := range v.Jobs {
		if j.Trigger.Root != root || j.Trigger.Audience != audience {
			continue
		}
		switch j.State {
		case "pending", "reserved":
			out.Pending++
		case "complete":
			out.Completed++
		case "failed":
			out.Failed++
		case "interrupted":
			out.Interrupted++
		}
		if j.Candidate != nil {
			copy := *j.Candidate
			copy.EventRefs = append([]string(nil), copy.EventRefs...)
			if copy.SkillDraft != nil {
				draft := *copy.SkillDraft
				copy.SkillDraft = &draft
			}
			c = append(c, copy)
		}
	}
	for _, t := range v.Tombstones {
		if t.Root != root || t.Audience != audience || t.Candidate == nil {
			continue
		}
		copy := *t.Candidate
		copy.EventRefs = append([]string(nil), copy.EventRefs...)
		if copy.SkillDraft != nil {
			draft := *copy.SkillDraft
			copy.SkillDraft = &draft
		}
		c = append(c, copy)
	}
	for _, r := range v.Reservations {
		if r.At.After(now.Add(-24 * time.Hour)) {
			out.CallsUsed++
			out.TokensReserved += r.Tokens
		}
	}
	return out, c, nil
}

// NewBackgroundReflector uses only the selected metadata. It cannot access tools,
// transcripts, hot memory or skills, and its output remains private review material.
func NewBackgroundReflector(client *llm.Client) BackgroundReflector {
	return func(ctx context.Context, t BackgroundTrigger) (*Candidate, error) {
		if client == nil {
			return nil, fmt.Errorf("background model unavailable")
		}
		input, err := json.Marshal(struct {
			Kind         TriggerKind `json:"kind"`
			Summary      string      `json:"summary"`
			EvidenceRefs []string    `json:"evidenceRefs"`
		}{t.Kind, t.Summary, t.EvidenceRefs})
		if err != nil {
			return nil, err
		}
		if len(input) > 8000 {
			return nil, fmt.Errorf("background input budget exceeded")
		}
		response, err := client.NewTurn().Complete(ctx, llm.Request{MaxTokens: 512, Messages: []llm.Message{{Role: "system", Content: reflectionPrompt + " For this background pass action must be new and relatedIds must be empty. Runtime failures support diagnosis proposals only, never a claim of verified task outcome."}, {Role: "user", Content: string(input)}}})
		if err != nil {
			return nil, err
		}
		if len(response.ToolCalls) > 0 || len(response.Content) > 4000 {
			return nil, fmt.Errorf("invalid background output")
		}
		var c *Candidate
		if err = json.Unmarshal([]byte(response.Content), &c); err != nil {
			return nil, fmt.Errorf("background output must be JSON candidate or null")
		}
		if c != nil {
			if c.SkillDraft != nil {
				return nil, fmt.Errorf("generic reflection cannot select skill target")
			}
			c.ID, c.Root, c.Audience, c.EventRefs = "", "", "", nil
			if err = validateBackgroundCandidate(c); err != nil {
				return nil, err
			}
		}
		return c, nil
	}
}
