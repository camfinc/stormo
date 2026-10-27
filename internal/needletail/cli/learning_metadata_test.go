package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/pkg/learning"
)

// Exercise actual example-instance build, classified nap blobs, dream reads and restore.
// Private outcome/reflection state survives reboot without entering tracked evidence.
func TestLearningMetadataNapRoundTrip(t *testing.T) {
	w := newLearningWorld(t)
	home := filepath.Join(w.tmp, "metadata-home-1")
	w.rehydrate(home)
	const procedure = "Acme ferry quotes require a deposit check before delivery."
	lesson := &learning.Learning{Kind: learning.KindMemory, Text: procedure}
	lesson.ID = learning.LearningID(lesson.Kind, lesson.Text)
	event := learning.OutcomeEvent{EventID: runtimelearn.Hash("agent-event"), TaskRoot: runtimelearn.Hash("task"), SubjectKind: "lesson", SubjectID: lesson.ID, SubjectRevision: learning.LessonRevision(lesson), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", Origin: runtimelearn.Hash("check"), ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
	rs := &runtimelearn.Store{Home: home}
	if err := rs.Append(event); err != nil {
		t.Fatal(err)
	}
	private := event
	private.EventID = runtimelearn.Hash("private-event")
	private.Audience = "dm:private-audience-sentinel"
	private.TaskRoot = runtimelearn.Hash("private-task")
	private.Origin = runtimelearn.Hash("private-check")
	if err := rs.Append(private); err != nil {
		t.Fatal(err)
	}
	// No model call is needed to test private queue persistence.
	reflection := []byte(`{"version":1,"jobs":{"private-job":"reserved"},"candidates":[],"privateSentinel":"private-reflection-sentinel"}`)
	writeMetadata := func(rel string, b []byte) {
		t.Helper()
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata(runtimelearn.ReflectionsFile, reflection)
	for _, path := range []string{runtimelearn.BackgroundFile, skills.OriginsFile, "state/needletail/skill-proposals-v1.json"} {
		writeMetadata(path, []byte(`{"version":1,"privateSentinel":"background-private-sentinel"}`))
	}
	proposal, _ := json.Marshal(map[string]any{"version": 1, "proposals": []runtimelearn.ExportProposal{{ID: runtimelearn.Hash("proposal"), Text: procedure, EventRefs: []string{event.EventID}, Audience: "agent", Kind: "procedure"}}})
	writeMetadata(runtimelearn.ProposalsFile, proposal)
	before := map[string][]byte{}
	for _, rel := range []string{runtimelearn.OutcomesFile, runtimelearn.OutcomesExportFile, runtimelearn.ReflectionsFile, runtimelearn.ProposalsFile, runtimelearn.BackgroundFile, skills.OriginsFile, "state/needletail/skill-proposals-v1.json"} {
		b, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			t.Fatal(err)
		}
		before[rel] = b
	}
	// Defense in depth: a mixed export from an older or hand-edited runtime must
	// still not cause dream to track private audience evidence.
	mixed, _ := json.Marshal(learning.EvidenceStore{Version: 1, Events: []learning.OutcomeEvent{event, private}})
	writeMetadata(runtimelearn.OutcomesExportFile, mixed)
	at := time.Date(2026, 10, 26, 11, 0, 0, 0, time.UTC)
	nap := w.nap(home, "metadata-1", at)
	classes := map[string]string{}
	for _, f := range nap.Files {
		classes[f.Path] = string(f.Class)
	}
	for rel, want := range map[string]string{runtimelearn.BackgroundFile: "raw", skills.OriginsFile: "raw", "state/needletail/skill-proposals-v1.json": "raw", runtimelearn.OutcomesFile: "raw", runtimelearn.ReflectionsFile: "raw", runtimelearn.OutcomesExportFile: "learning", runtimelearn.ProposalsFile: "learning"} {
		if classes[rel] != want {
			t.Fatalf("class %s=%q want %s", rel, classes[rel], want)
		}
	}
	report, reads := w.dream()
	if report.NewLearnings != 1 {
		t.Fatalf("dream=%+v", report)
	}
	for _, key := range reads {
		if strings.Contains(key, "/raw/") {
			t.Fatalf("dream read private blob: %s", key)
		}
	}
	evidence, err := learning.LoadEvidence(w.root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Events) != 1 || evidence.Events[0].EventID != event.EventID {
		t.Fatalf("export evidence=%+v", evidence.Events)
	}
	ledger, err := learning.LoadLedger(w.root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 1 || ledger[0].Status != learning.Proposed || ledger[0].Evidence.Confirmations != 1 {
		t.Fatalf("ledger=%+v", ledger)
	}
	// Review rejects it; the restored proposal replay must preserve the rejection.
	ledger[0].Status = learning.Rejected
	if err := learning.SaveLedger(w.root, "atlas", ledger); err != nil {
		t.Fatal(err)
	}
	w.rebuild()
	home2 := filepath.Join(w.tmp, "metadata-home-2")
	w.rehydrate(home2)
	for rel, want := range before {
		if rel == runtimelearn.OutcomesExportFile {
			continue
		}
		p := filepath.Join(home2, rel)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("restore %s: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Fatalf("restore changed %s", rel)
		}
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0600 {
			t.Errorf("restored private metadata %s mode=%v", rel, st.Mode().Perm())
		}
	}
	// The agent export is derived, so replay must regenerate it after reboot without
	// appending a duplicate raw event.
	if err := (&runtimelearn.Store{Home: home2}).Append(event); err != nil {
		t.Fatal(err)
	}
	exported, err := os.ReadFile(filepath.Join(home2, runtimelearn.OutcomesExportFile))
	if err != nil {
		t.Fatal(err)
	}
	if string(exported) != string(before[runtimelearn.OutcomesExportFile]) {
		t.Fatal("derived export changed after idempotent restart replay")
	}
	w.nap(home2, "metadata-2", at.Add(time.Minute))
	w.dream()
	ledger, err = learning.LoadLedger(w.root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 1 || ledger[0].Status != learning.Rejected || ledger[0].Evidence.Confirmations != 1 {
		t.Fatalf("replay resurrected/duplicated lesson: %+v", ledger)
	}
	evidence, err = learning.LoadEvidence(w.root, "atlas")
	if err != nil || len(evidence.Events) != 1 {
		t.Fatalf("duplicate evidence after restore: %+v %v", evidence, err)
	}
}
