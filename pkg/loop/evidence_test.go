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

// The Hermes path: a memory entry seen clean in one nap and flagged in another (the same scrubbed
// text) is flagged in the ledger, whichever came first.
func TestDreamLedgerKeepsFlagsAcrossHermesSightings(t *testing.T) {
	inst := reviewInstance(t)
	root := inst.Root
	store := FsStore{Root: t.TempDir()}
	putNap := func(id, memory string) {
		t.Helper()
		body := []byte(memory)
		sha := build.Sha256(body)
		if err := store.Put(BlobKey("atlas", engine.Learning, sha), body); err != nil {
			t.Fatal(err)
		}
		nap := Nap{ID: id, Agent: "atlas", Unit: "sales", Engine: "hermes", Instance: "instance", TakenAt: "2026-10-26T10:00:00Z",
			Files: []NapFile{{Path: "memories/MEMORY.md", Class: engine.Learning, Sha256: sha, Size: len(body)}}}
		b, _ := json.Marshal(nap)
		if err := store.Put(NapKey("atlas", id), b); err != nil {
			t.Fatal(err)
		}
	}
	putNap("nap-a", "Use the token [SECRET] for the CRM.")
	putNap("nap-b", "Use the token sk_abcdefghijklmnop for the CRM.")
	if _, err := Dream(inst, "atlas", store); err != nil {
		t.Fatal(err)
	}
	ledger, err := learning.LoadLedger(root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range ledger {
		if strings.Contains(e.Text, "for the CRM") {
			found = true
			if !slices.Contains(e.PII, "secret") || e.SeenCount != 2 {
				t.Errorf("entry %+v lost its flag across sightings", e)
			}
		}
	}
	if !found {
		t.Fatalf("no ledger entry: %+v", ledger)
	}
}

// A merge the evidence store refuses does not lose the evidence: the evidence watermark stays
// behind while the nap watermark moves, and the next dream (even with no new nap) merges it.
func TestDreamRetriesUnmergedEvidence(t *testing.T) {
	eng := withLearningEngine(t)
	inst := reviewInstance(t)
	root := inst.Root
	store := FsStore{Root: t.TempDir()}
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
	event := learning.OutcomeEvent{EventID: shaText("pending"), TaskRoot: shaText("task"), Origin: shaText("check"), SubjectKind: "skill", SubjectID: shaText("notes/check"), SubjectRevision: shaText("rev"), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
	body, _ := json.Marshal(learning.EvidenceStore{Version: 1, Events: []learning.OutcomeEvent{event}})
	sha := build.Sha256(body)
	if err := store.Put(BlobKey("atlas", engine.Learning, sha), body); err != nil {
		t.Fatal(err)
	}
	nap := Nap{ID: "nap-a", Agent: "atlas", Unit: "sales", Engine: "hermes", Instance: "instance", TakenAt: "2026-10-26T10:00:00Z",
		Files: []NapFile{{Path: eng.OutcomePath(), Class: engine.Learning, Sha256: sha, Size: len(body)}}}
	b, _ := json.Marshal(nap)
	if err := store.Put(NapKey("atlas", "nap-a"), b); err != nil {
		t.Fatal(err)
	}
	if _, err := Dream(inst, "atlas", store); err != nil {
		t.Fatal(err)
	}
	wm, _ := LoadWatermark(root, "atlas")
	if wm.LastNap == nil || *wm.LastNap != "nap-a" || wm.EvidenceNap == nil || *wm.EvidenceNap != "" {
		t.Fatalf("watermarks after a refused merge: last %v evidence %v", wm.LastNap, wm.EvidenceNap)
	}
	// Pruning keeps the nap whose evidence is still pending.
	if _, err := Prune(inst, "atlas", store, 1); err != nil {
		t.Fatal(err)
	}
	if ok, _ := ReadJSON(store, NapKey("atlas", "nap-a"), &Nap{}); !ok {
		t.Fatal("prune dropped the nap whose evidence is pending")
	}
	// Room again: the next dream, with no new nap, merges the pending evidence.
	if err := learning.SaveEvidence(root, "atlas", &learning.EvidenceStore{Version: learning.EvidenceVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := Dream(inst, "atlas", store); err != nil {
		t.Fatal(err)
	}
	if after, _ := learning.LoadEvidence(root, "atlas"); len(after.Events) != 1 || after.Events[0].EventID != event.EventID {
		t.Fatalf("pending evidence not merged: %+v", after.Events)
	}
	wm, _ = LoadWatermark(root, "atlas")
	if wm.EvidenceNap == nil || *wm.EvidenceNap != "nap-a" {
		t.Fatalf("evidence watermark did not move after the merge: %v", wm.EvidenceNap)
	}
}

// Item 19 (Claude L5): evidence whose merge keeps being refused does not hold every later nap
// forever: after MaxEvidenceRetries dreams (or MaxEvidenceAge since the first refusal) the
// evidence watermark catches up with a loud notice, and pruning goes on. Hermes has no outcome
// evidence and never gets here.
func TestDreamGivesUpOnStuckEvidence(t *testing.T) {
	eng := withLearningEngine(t)
	inst := reviewInstance(t)
	root := inst.Root
	store := FsStore{Root: t.TempDir()}
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
	event := learning.OutcomeEvent{EventID: shaText("stuck"), TaskRoot: shaText("task"), Origin: shaText("check"), SubjectKind: "skill", SubjectID: shaText("notes/check"), SubjectRevision: shaText("rev"), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
	body, _ := json.Marshal(learning.EvidenceStore{Version: 1, Events: []learning.OutcomeEvent{event}})
	sha := build.Sha256(body)
	if err := store.Put(BlobKey("atlas", engine.Learning, sha), body); err != nil {
		t.Fatal(err)
	}
	nap := Nap{ID: "nap-a", Agent: "atlas", Unit: "sales", Engine: "hermes", Instance: "instance", TakenAt: "2026-10-26T10:00:00Z",
		Files: []NapFile{{Path: eng.OutcomePath(), Class: engine.Learning, Sha256: sha, Size: len(body)}}}
	b, _ := json.Marshal(nap)
	if err := store.Put(NapKey("atlas", "nap-a"), b); err != nil {
		t.Fatal(err)
	}
	var notices []string
	for range MaxEvidenceRetries + 1 {
		r, err := Dream(inst, "atlas", store)
		if err != nil {
			t.Fatal(err)
		}
		notices = append(notices, r.Notices...)
	}
	wm, _ := LoadWatermark(root, "atlas")
	if wm.EvidenceNap != nil && *wm.EvidenceNap != "nap-a" {
		t.Fatalf("the evidence watermark is still stuck after %d dreams: %v", MaxEvidenceRetries+1, *wm.EvidenceNap)
	}
	if !slices.ContainsFunc(notices, func(n string) bool { return strings.Contains(n, "OUTCOME EVIDENCE DROPPED") }) {
		t.Errorf("no loud notice: %v", notices)
	}
	if dream := read(t, filepath.Join(root, "agents", "atlas", "learnings", "DREAM.md")); !strings.Contains(dream, "OUTCOME EVIDENCE DROPPED") {
		t.Errorf("DREAM.md does not say so:\n%s", dream)
	}
}
