package learning

import (
	"os"
	"strings"
	"testing"
)

func testOutcome(id, task, origin string) OutcomeEvent {
	return OutcomeEvent{EventID: id, TaskRoot: task, Origin: origin, SubjectKind: "lesson", SubjectID: "lesson", SubjectRevision: "revision", At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
}
func TestEvidenceRoundTripAndReplay(t *testing.T) {
	root := t.TempDir()
	event := testOutcome("event", "task", "check")
	if err := MergeEvidence(root, "atlas", []OutcomeEvent{event, event}); err != nil {
		t.Fatal(err)
	}
	if err := MergeEvidence(root, "atlas", []OutcomeEvent{event}); err != nil {
		t.Fatal(err)
	}
	s, err := LoadEvidence(root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	retry := testOutcome("retry", "task", "check2")
	s.Ingest(retry)
	reused := testOutcome("reused", "other-task", "check")
	s.Ingest(reused)
	summary := s.Summary("lesson", "lesson", "revision", "agent")
	if summary.Confirmations != 1 {
		t.Fatalf("duplicate confirmations %+v", summary)
	}
	if s.Summary("lesson", "lesson", "revision", "private").Confirmations != 0 || s.Summary("lesson", "lesson", "new-revision", "agent").Confirmations != 0 {
		t.Fatal("revision/audience leaked")
	}
	if _, err = s.Ingest(OutcomeEvent{}); err == nil {
		t.Fatal("invalid event accepted")
	}
	event.TaskOutcome = "failed"
	if _, err = s.Ingest(event); err == nil {
		t.Fatal("ID collision accepted")
	}
	st, _ := os.Stat(EvidencePath(root, "atlas"))
	if st.Mode().Perm() != 0600 {
		t.Fatalf("permissions %v", st.Mode())
	}
}
func TestEvidenceDoesNotInventSuccess(t *testing.T) {
	s := &EvidenceStore{Version: EvidenceVersion}
	for _, source := range []string{"snapshot", "view", "model", "runtime"} {
		e := testOutcome(source, source, source)
		e.SourceType = source
		s.Ingest(e)
	}
	e := testOutcome("failure", "failed-task", "assertion")
	e.TaskOutcome = "failed"
	s.Ingest(e)
	summary := s.Summary("lesson", "lesson", "revision", "agent")
	if summary.Confirmations != 0 || summary.Failures != 1 || summary.AutoEligible(1) {
		t.Fatalf("false success %+v", summary)
	}
	if strings.Contains(summary.Revision, "task") {
		t.Fatal("summary leaks task identity")
	}
}
func TestLegacyEvidenceMigrationAndSightings(t *testing.T) {
	root := t.TempDir()
	entries := []*Learning{}
	for i := 0; i < 10; i++ {
		Upsert(&entries, Sighting{Kind: KindMemory, Text: "Stable note", Agent: "atlas", Nap: string(rune('a' + i)), At: "2026-10-26T10:00:00Z"})
	}
	entries[0].Status = Accepted
	if err := SaveLedger(root, "atlas", entries); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadLedger(root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	e := loaded[0]
	if e.SeenCount != 10 || e.Evidence.Confirmations != 0 || !e.Evidence.Unknown || e.Status != Accepted {
		t.Fatalf("migration changed semantics %+v", e)
	}
	s := &EvidenceStore{Version: EvidenceVersion}
	event := testOutcome("event", "task", "check")
	event.SubjectID = e.ID
	event.SubjectRevision = LessonRevision(e)
	s.Ingest(event)
	SaveEvidence(root, "atlas", s)
	loaded, _ = LoadLedger(root, "atlas")
	if loaded[0].Evidence.Confirmations != 1 {
		t.Fatal("exact evidence not attached")
	}
	loaded[0].Text = "Stable Note"
	SaveLedger(root, "atlas", loaded)
	loaded, _ = LoadLedger(root, "atlas")
	if loaded[0].Evidence.Confirmations != 0 {
		t.Fatal("rewrite inherited evidence")
	}
}
func TestUnknownEvidenceVersionPreserved(t *testing.T) {
	root := t.TempDir()
	p := EvidencePath(root, "atlas")
	os.MkdirAll(strings.TrimSuffix(p, "/evidence-v1.json"), 0755)
	body := []byte(`{"version":2,"events":[]}`)
	os.WriteFile(p, body, 0600)
	if err := MergeEvidence(root, "atlas", []OutcomeEvent{testOutcome("event", "task", "check")}); err == nil {
		t.Fatal("future store overwritten")
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(body) {
		t.Fatal("future evidence lost")
	}
}
