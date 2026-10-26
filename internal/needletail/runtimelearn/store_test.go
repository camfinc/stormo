package runtimelearn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/learning"
)

func event() learning.OutcomeEvent {
	return learning.OutcomeEvent{EventID: Hash("event"), TaskRoot: Hash("task"), SubjectKind: "skill", SubjectID: Hash("skill"), SubjectRevision: Hash("rev"), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "runtime", Origin: Hash("call"), ExecutionStatus: "succeeded", TaskOutcome: "unknown"}
}
func TestOutcomeRestartAudienceAndFeedback(t *testing.T) {
	s := &Store{Home: t.TempDir()}
	e := event()
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	s = &Store{Home: s.Home}
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Events(e.TaskRoot, "agent")
	if len(got) != 1 || got[0].TaskOutcome != "unknown" {
		t.Fatal(got)
	}
	if _, err := s.Feedback(e.TaskRoot, "other", e.EventID, "succeeded", "check1"); err == nil {
		t.Fatal("cross audience feedback")
	}
	for i := 0; i < 2; i++ {
		if _, err := s.Feedback(e.TaskRoot, e.Audience, e.EventID, "failed", "check1"); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = s.Events(e.TaskRoot, "agent")
	if len(got) != 2 || got[1].TaskOutcome != "failed" || got[1].ExecutionStatus != "succeeded" {
		t.Fatal(got)
	}
	e.EventID = Hash("private")
	e.Audience = "dm:person"
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.Home, OutcomesExportFile))
	if strings.Contains(string(raw), "dm:person") {
		t.Fatal("private export")
	}
	info, _ := os.Stat(filepath.Join(s.Home, OutcomesFile))
	if info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode())
	}
}
func TestUnknownVersionPreserved(t *testing.T) {
	s := &Store{Home: t.TempDir()}
	if err := s.write(OutcomesFile, map[string]any{"version": 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(event()); err == nil {
		t.Fatal("unknown version overwritten")
	}
	b, _ := os.ReadFile(filepath.Join(s.Home, OutcomesFile))
	if string(b) != "{\"version\":2}" {
		t.Fatal(string(b))
	}
}

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestReflectionNoToolsBudgetNoopRestartAndPrivate(t *testing.T) {
	s := &Store{Home: t.TempDir()}
	e := event()
	e.TaskOutcome = "corrected"
	e.SourceType = "authorized_feedback"
	_ = s.Append(e)
	calls := 0
	client := &llm.Client{Endpoints: []llm.Endpoint{{BaseURL: "http://synthetic", Model: "fixture"}}, HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["max_tokens"] != float64(512) || body["tools"] != nil {
			t.Fatalf("unbounded or tools: %v", body)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"null\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))}, nil
	})}}
	for i := 0; i < 2; i++ {
		if err := s.Reflect(context.Background(), client, nil, "session", e, "Selected check found an obsolete retry limit.", nil); err != nil {
			t.Fatal(err)
		}
		s = &Store{Home: s.Home}
	}
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
	c, _ := s.Candidates(e.TaskRoot, e.Audience)
	if len(c) != 0 {
		t.Fatal(c)
	}
	if err := s.Export(e.TaskRoot, "dm:person", "candidate", nil); err == nil {
		t.Fatal("private promotion")
	}
}

func TestConcurrentFeedbackDelivery(t *testing.T) {
	s := &Store{Home: t.TempDir()}
	e := event()
	if err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			_, err := s.Feedback(e.TaskRoot, e.Audience, e.EventID, "succeeded", "same-check")
			errs <- err
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Events(e.TaskRoot, e.Audience)
	if len(got) != 2 {
		t.Fatal(got)
	}
}
func TestReflectionCandidateExportRetainsReviewContext(t *testing.T) {
	s := &Store{Home: t.TempDir()}
	e := event()
	e.SourceType = "authorized_feedback"
	e.TaskOutcome = "corrected"
	_ = s.Append(e)
	client := &llm.Client{Endpoints: []llm.Endpoint{{BaseURL: "http://synthetic", Model: "fixture"}}, HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		c := Candidate{Text: "Check the return code before retrying.", Checks: "A task assertion caught the wrong retry condition.", Conditions: "Only synchronous commands.", Exceptions: "Background commands need later checks.", Sensitivity: "public", ReviewDate: "2026-11-26", Action: "new", RelatedIDs: []string{}}
		content, _ := json.Marshal(c)
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": string(content)}, "finish_reason": "stop"}}})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + string(chunk) + "\n\ndata: [DONE]\n\n"))}, nil
	})}}
	if err := s.Reflect(context.Background(), client, nil, "session", e, "Selected task assertion failed then was corrected.", nil); err != nil {
		t.Fatal(err)
	}
	candidates, _ := s.Candidates(e.TaskRoot, e.Audience)
	if len(candidates) != 1 {
		t.Fatal(candidates)
	}
	if _, err := os.Stat(filepath.Join(s.Home, ProposalsFile)); !os.IsNotExist(err) {
		t.Fatal("candidate exported automatically")
	}
	if err := s.Export(e.TaskRoot, e.Audience, candidates[0].ID, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(s.Home, ProposalsFile))
	if !strings.Contains(string(b), "synchronous commands") || !strings.Contains(string(b), "Proposal action") || !strings.Contains(string(b), e.EventID) {
		t.Fatal(string(b))
	}
	if _, err := os.Stat(filepath.Join(s.Home, "memories/MEMORY.md")); !os.IsNotExist(err) {
		t.Fatal("hot memory mutation")
	}
	// A handler may taint the session while the model is responding. Its final
	// candidate write must check authority atomically with that session append.
	sessionStore, err := sessions.Open(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sessionStore.Create(wire.Session{ID: "tainted"}); err != nil {
		t.Fatal(err)
	}
	blocked := &Store{Home: t.TempDir(), Trusted: sessionStore.WithTrusted}
	_ = blocked.Append(e)
	previous := client.HTTP.Transport
	client.HTTP.Transport = transport(func(r *http.Request) (*http.Response, error) {
		if _, err := sessionStore.Append("tainted", wire.Message{Role: "assistant", Content: "External text", Untrusted: true}); err != nil {
			t.Fatal(err)
		}
		return previous.RoundTrip(r)
	})
	if err := blocked.Reflect(context.Background(), client, nil, "tainted", e, "Selected check", nil); err == nil {
		t.Fatal("tainted reflection candidate persisted")
	}
	none, _ := blocked.Candidates(e.TaskRoot, e.Audience)
	if len(none) != 0 {
		t.Fatal(none)
	}

}
