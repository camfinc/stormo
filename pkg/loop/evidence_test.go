package loop

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestDreamOutcomeExportReplayAndSkillReview(t *testing.T) {
	inst := reviewInstance(t)
	root := inst.Root
	write(t, filepath.Join(manifest.AgentDir(root, "atlas"), "agent.yaml"), "format: 1\nid: atlas\nname: Atlas\nunit: sales\nrole: test\nengine:\n  kind: needletail\n  version: 0.1.0\nmodel:\n  name: test\n")
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
	outcomePath := "state/needletail/outcomes-export-v1.json"
	putNap := func(id string, events []learning.OutcomeEvent) {
		t.Helper()
		body, _ := json.Marshal(learning.EvidenceStore{Version: 1, Events: events})
		sha := build.Sha256(body)
		if err := store.Put(BlobKey("atlas", engine.Learning, sha), body); err != nil {
			t.Fatal(err)
		}
		store.Put(BlobKey("atlas", engine.Learning, skillSHA), skillBody)
		nap := Nap{ID: id, Agent: "atlas", Unit: "sales", Engine: "needletail", Instance: "instance", TakenAt: "2026-10-26T10:00:00Z", Files: []NapFile{{Path: outcomePath, Class: engine.Learning, Sha256: sha, Size: len(body)}, {Path: skillPath, Class: engine.Learning, Sha256: skillSHA, Size: len(skillBody)}}}
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
	if strings.Contains(cands[0].Body, "private-client") || strings.Contains(cands[0].Body, event.TaskRoot) {
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
	if err != nil || len(cands) != 0 || len(held) != 1 {
		t.Fatalf("contradiction failed to suspend skill %v %v %v", cands, held, err)
	}
	if err := Approve(inst, "atlas", old, Decider{By: "test"}); err == nil {
		t.Fatal("stale cached skill approval applied")
	}
}
