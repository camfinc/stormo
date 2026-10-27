package loop

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
)

func TestDreamOutcomeExportReplayAndSkillReview(t *testing.T) {
	eng := withLearningEngine(t)
	inst := reviewInstance(t)
	root := inst.Root
	store := FsStore{Root: t.TempDir()}
	skillPath := "skills/notes/check/SKILL.md"
	skillBody := []byte("---\nname: check\ndescription: Check results\n---\nCheck the task assertion.\n")
	skillSHA := build.Sha256(skillBody)
	revision := build.SkillHash(map[string]string{skillPath: skillSHA}, []string{skillPath})
	event := learning.OutcomeEvent{EventID: shaText("event"), TaskRoot: shaText("task"), Origin: shaText("check"), SubjectKind: "skill", SubjectID: shaText("notes/check"), SubjectRevision: revision, At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "authorized_feedback", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
	private := event
	private.EventID = shaText("private")
	private.Audience = "dm:private-client-identity"
	invalid := event
	invalid.EventID = "a-private-source-name"
	invalid.Origin = "unflagged-private-identity"
	outcomePath := eng.OutcomePath()
	putNap := func(id string, events []learning.OutcomeEvent) {
		t.Helper()
		body, _ := json.Marshal(learning.EvidenceStore{Version: 1, Events: events})
		sha := build.Sha256(body)
		if err := store.Put(BlobKey("atlas", engine.Learning, sha), body); err != nil {
			t.Fatal(err)
		}
		store.Put(BlobKey("atlas", engine.Learning, skillSHA), skillBody)
		nap := Nap{ID: id, Agent: "atlas", Unit: "sales", Engine: "hermes", Instance: "instance", TakenAt: "2026-10-26T10:00:00Z", Files: []NapFile{{Path: outcomePath, Class: engine.Learning, Sha256: sha, Size: len(body)}, {Path: skillPath, Class: engine.Learning, Sha256: skillSHA, Size: len(skillBody)}}}
		b, _ := json.Marshal(nap)
		if err := store.Put(NapKey("atlas", id), b); err != nil {
			t.Fatal(err)
		}
	}
	putNap("nap-a", []learning.OutcomeEvent{event, private, invalid})
	if _, err := Dream(inst, "atlas", store); err != nil {
		t.Fatal(err)
	}
	evidence, err := learning.LoadEvidence(root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Events) != 1 {
		t.Fatalf("unsafe exported provenance: %+v", evidence)
	}
	cands, _, err := ReviewCandidates(inst, "atlas", 1)
	if err != nil || len(cands) != 1 || cands[0].Kind != KindSkill {
		t.Fatalf("skill outcome disconnected: %v %v", cands, err)
	}
	if c := cands[0]; strings.Contains(c.Body+c.Evidence, "private-client") || strings.Contains(c.Body+c.Evidence, event.TaskRoot) || !strings.Contains(c.Evidence, "Independent confirmations (a task check or authorized feedback found the result right): 1") {
		t.Fatal("review summary exposes provenance")
	}
	old := cands[0]
	putNap("nap-b", []learning.OutcomeEvent{event})
	if _, err := Dream(inst, "atlas", store); err != nil {
		t.Fatal(err)
	}
	evidence, _ = learning.LoadEvidence(root, "atlas")
	if len(evidence.Events) != 1 {
		t.Fatal("nap replay duplicated event")
	}
	negative := event
	negative.EventID = shaText("negative")
	negative.TaskRoot = shaText("failed-task")
	negative.Origin = shaText("failure-check")
	negative.TaskOutcome = "failed"
	putNap("nap-c", []learning.OutcomeEvent{event, negative})
	if _, err := Dream(inst, "atlas", store); err != nil {
		t.Fatal(err)
	}
	cands, held, err := ReviewCandidates(inst, "atlas", 1)
	if err != nil || len(cands) != 1 || len(held) != 0 || !strings.HasPrefix(cands[0].Evidence, "WARNING: this exact version has 1 recorded failure(s)") {
		t.Fatalf("a contradicted skill is not presented to the model as one: %v %v %v", cands, held, err)
	}
	if err := Approve(inst, "atlas", old, Decider{By: "test"}); err == nil {
		t.Fatal("stale cached skill approval applied")
	}
}

// Nothing an engine exports for learning makes the dream fail: a bad reflection export or proposal,
// a bad outcome export, or an evidence store that refuses the merge (full) is reported in the
// dream's notices (and DREAM.md), and the rest of the dream goes through, watermark included.
func TestDreamReportsLearningExportsItCannotTake(t *testing.T) {
	eng := withLearningEngine(t)
	inst := reviewInstance(t)
	root := inst.Root
	store := FsStore{Root: t.TempDir()}

	// A full evidence store: the merge refuses any new event.
	full := &learning.EvidenceStore{Version: learning.EvidenceVersion}
	for i := range learning.MaxOutcomeEvents {
		e := learning.OutcomeEvent{EventID: shaText(fmt.Sprint("full", i)), TaskRoot: shaText("t"), Origin: shaText("o"), SubjectKind: "lesson", SubjectID: "aaaaaaaaaaaa", SubjectRevision: shaText("r"), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
		if _, err := full.Ingest(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := learning.SaveEvidence(root, "atlas", full); err != nil {
		t.Fatal(err)
	}

	putBlob := func(body []byte) string {
		t.Helper()
		sha := build.Sha256(body)
		if err := store.Put(BlobKey("atlas", engine.Learning, sha), body); err != nil {
			t.Fatal(err)
		}
		return sha
	}
	putNap := func(id string, files map[string][]byte) {
		t.Helper()
		nap := Nap{ID: id, Agent: "atlas", Unit: "sales", Engine: "hermes", Instance: "instance", TakenAt: "2026-10-26T10:00:00Z"}
		for path, body := range files {
			nap.Files = append(nap.Files, NapFile{Path: path, Class: engine.Learning, Sha256: putBlob(body), Size: len(body)})
		}
		b, _ := json.Marshal(nap)
		if err := store.Put(NapKey("atlas", id), b); err != nil {
			t.Fatal(err)
		}
	}
	proposals, _ := json.Marshal(map[string]any{"version": 1, "proposals": []any{
		map[string]any{"id": "a", "text": "Check the quote currency before presenting a total.", "eventRefs": []string{}, "audience": "agent", "kind": "procedure"},
		map[string]any{"id": "b", "text": "Send the quote to client@example.com first.", "eventRefs": []string{}, "audience": "agent", "kind": "procedure"},
		map[string]any{"id": "c", "text": "A private note.", "eventRefs": []string{}, "audience": "dm:someone", "kind": "procedure"},
	}})
	event := learning.OutcomeEvent{EventID: shaText("new"), TaskRoot: shaText("task"), Origin: shaText("check"), SubjectKind: "skill", SubjectID: shaText("notes/check"), SubjectRevision: shaText("rev"), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
	outcomes, _ := json.Marshal(learning.EvidenceStore{Version: 1, Events: []learning.OutcomeEvent{event}})
	putNap("nap-a", map[string][]byte{eng.LearningProposalsPath(): proposals, eng.OutcomePath(): []byte("{not json")})
	putNap("nap-b", map[string][]byte{eng.LearningProposalsPath(): []byte(`{"version":9,"proposals":[]}`), eng.OutcomePath(): outcomes})

	rep, err := Dream(inst, "atlas", store)
	if err != nil {
		t.Fatalf("the dream failed: %v", err)
	}
	if len(rep.Notices) != 5 {
		t.Errorf("notices %q", rep.Notices)
	}
	for _, want := range []string{"nap-a: outcome evidence", "nap-a: reflection proposal 2", "nap-a: reflection proposal 3", "nap-b: reflection proposals", "outcome evidence not merged"} {
		if !slices.ContainsFunc(rep.Notices, func(n string) bool { return strings.Contains(n, want) }) {
			t.Errorf("no notice %q in %q", want, rep.Notices)
		}
	}
	ledger, err := learning.LoadLedger(root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	clean, flagged := 0, 0
	for _, e := range ledger {
		switch {
		case strings.Contains(e.Text, "private note"):
			t.Errorf("a private proposal reached the ledger: %+v", e)
		case len(e.PII) > 0 && !strings.Contains(e.Text, "example.com"):
			flagged++
		case strings.HasPrefix(e.Text, "Check the quote currency"):
			clean++
		}
	}
	if clean != 1 || flagged != 1 {
		t.Errorf("ledger kept %d clean and %d flagged proposals: %+v", clean, flagged, ledger)
	}
	if wm, _ := LoadWatermark(root, "atlas"); wm.LastNap == nil || *wm.LastNap != "nap-b" {
		t.Errorf("watermark did not move past the naps: %+v", wm.LastNap)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "agents/atlas/learnings/DREAM.md")); !strings.Contains(string(b), "outcome evidence not merged") {
		t.Errorf("DREAM.md has no notices:\n%s", b)
	}
	if after, _ := learning.LoadEvidence(root, "atlas"); len(after.Events) != learning.MaxOutcomeEvents {
		t.Errorf("the full store changed: %d events", len(after.Events))
	}
}
