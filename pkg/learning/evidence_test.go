package learning

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
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

// Without outcome evidence (every Hermes lesson) the hot tier ranks as it did before evidence
// existed: pinned, then most corroborated (seen), then most recent, then id.
func TestRankWithoutEvidenceFallsBackToSightings(t *testing.T) {
	pinned := true
	mk := func(id string, seen int, last string) *Learning {
		return &Learning{ID: id, SeenCount: seen, LastSeen: last}
	}
	entries := []*Learning{
		mk("a00000000001", 1, "2026-10-01T00:00:00.000Z"),
		mk("b00000000002", 3, "2026-10-02T00:00:00.000Z"),
		mk("c00000000003", 3, "2026-10-05T00:00:00.000Z"),
		mk("d00000000004", 2, "2026-10-09T00:00:00.000Z"),
		mk("e00000000005", 1, "2026-10-01T00:00:00.000Z"),
	}
	entries[4].Pinned = &pinned
	empty := (&EvidenceStore{Version: EvidenceVersion}).Summary("lesson", "x", "r", "agent")
	for _, e := range entries {
		e.Evidence = empty
	}
	sort.SliceStable(entries, func(i, j int) bool { return Rank(entries[i], entries[j]) })
	got := []string{}
	for _, e := range entries {
		got = append(got, e.ID[:1])
	}
	if strings.Join(got, "") != "ecbda" {
		t.Fatalf("rank without evidence = %v, want pinned, seen desc, last seen desc, id", got)
	}

	// Supported knowledge still comes first among the unpinned.
	entries[4].Evidence = EvidenceSummary{Confirmations: 1, EvidenceTypes: []string{"assertion"}} // "a", seen once
	sort.SliceStable(entries, func(i, j int) bool { return Rank(entries[i], entries[j]) })
	if entries[1].ID[:1] != "a" {
		t.Fatalf("a confirmed lesson ranks below unconfirmed ones: %v", entries[1].ID)
	}
}

// An evidence store the readers cannot read (corrupt, oversized, a version this build does not
// know) never breaks the ledger, the compile or the review for the agent: it reads as no evidence,
// with a warning. Only MergeEvidence, which would overwrite it, refuses.
func TestUnreadableEvidenceReadsAsNone(t *testing.T) {
	inst := &instance.Instance{Root: t.TempDir()}
	root := inst.Root
	manifestDir := filepath.Join(root, "agents", "atlas")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "units", "sales"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "units", "sales", "unit.yaml"), []byte("id: sales\nname: Sales\ndescription: Sales\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "SOUL.md"), []byte("# Atlas\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "agent.yaml"), []byte("format: 1\nid: atlas\nname: Atlas\nunit: sales\nrole: test\nengine:\n  kind: hermes\n  version: 0.1.0\nmodel:\n  name: test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []*Learning{}
	Upsert(&entries, Sighting{Kind: KindMemory, Text: "Reports are due on Mondays.", Agent: "atlas", Nap: "n", At: "2026-10-26T10:00:00Z"})
	entries[0].Status = Accepted
	if err := SaveLedger(root, "atlas", entries); err != nil {
		t.Fatal(err)
	}
	p := EvidencePath(root, "atlas")
	for name, body := range map[string]string{
		"corrupt":   "{not json",
		"future":    `{"version":2,"events":[]}`,
		"oversized": strings.Repeat(" ", MaxEvidenceBytes+1),
		"invalid":   `{"version":1,"events":[{"eventId":"x"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			s, warning := ReadEvidence(root, "atlas")
			if s == nil || len(s.Events) != 0 || warning == "" {
				t.Fatalf("ReadEvidence = %v, %q", s, warning)
			}
			loaded, err := LoadLedger(root, "atlas")
			if err != nil || len(loaded) != 1 || loaded[0].Evidence.Confirmations != 0 {
				t.Fatalf("LoadLedger = %v, %v", loaded, err)
			}
			if _, err := CompileLearning(inst, "atlas", engine.Delimited("\n")); err != nil {
				t.Fatalf("CompileLearning: %v", err)
			}
			if err := MergeEvidence(root, "atlas", []OutcomeEvent{testOutcome("event", "task", "check")}); err == nil {
				t.Fatal("MergeEvidence overwrote a store it cannot read")
			}
			if got, _ := os.ReadFile(p); string(got) != body {
				t.Fatal("the unreadable store was changed")
			}
		})
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, warning := ReadEvidence(root, "atlas"); warning != "" {
		t.Fatalf("a missing store is no warning: %q", warning)
	}
}
