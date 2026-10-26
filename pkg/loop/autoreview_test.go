package loop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine/hermes/hermestest"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

func reviewInstance(t *testing.T) *instance.Instance {
	t.Helper()
	root := filepath.Join(t.TempDir(), "instance")
	example := hermestest.Instance(t)
	for _, d := range []string{"units", "bridge", "agents/atlas"} {
		copyDir(t, filepath.Join(example, d), filepath.Join(root, d))
	}
	write(t, filepath.Join(root, "stormo.yaml"), read(t, filepath.Join(example, "stormo.yaml")))
	os.RemoveAll(filepath.Join(root, "agents", "atlas", "learnings"))
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// The rules hold what needs a person and let only the rest reach the review's model.
func TestReviewRules(t *testing.T) {
	inst := reviewInstance(t)
	root := inst.Root
	agent, err := manifest.Load(root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	scrubber := learning.NewScrubber(inst)

	ledger := []*learning.Learning{}
	sight := func(kind learning.Kind, raw string) *learning.Learning {
		text, pii := scrubber.Scrub(raw)
		e, _ := learning.Upsert(&ledger, learning.Sighting{Kind: kind, Text: text, PII: pii, Agent: "atlas", Unit: "sales", Instance: "i", Nap: "n", At: "2026-10-19T10:00:00.000Z"})
		return e
	}
	clean := sight(learning.KindMemory, "Deals close on Fridays.")
	user := sight(learning.KindUser, "The owner wants voice replies.")
	pii := sight(learning.KindMemory, "Call Dana at +1 415 555 0123.")
	if err := learning.SaveLedger(root, "atlas", ledger); err != nil {
		t.Fatal(err)
	}

	evidence := &learning.EvidenceStore{Version: learning.EvidenceVersion}
	evidence.Ingest(learning.OutcomeEvent{EventID: "check", TaskRoot: "task", Origin: "check", SubjectKind: "lesson", SubjectID: clean.ID, SubjectRevision: learning.LessonRevision(clean), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"})
	evidence.Ingest(learning.OutcomeEvent{EventID: "skill-check", TaskRoot: "skill-task", Origin: "skill-check", SubjectKind: "skill", SubjectID: shaText("notes/brief"), SubjectRevision: "h1", At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"})
	if err := learning.SaveEvidence(root, "atlas", evidence); err != nil {
		t.Fatal(err)
	}
	skills := ProposalsDir(root, "atlas")
	write(t, filepath.Join(skills, "notes/brief/SKILL.md"), "---\nname: brief\n---\nKeep briefs to five lines.\n")
	write(t, filepath.Join(skills, "notes/brief/.proposal.json"), `{"skill":"notes/brief","napHash":"h1","nap":"n","pii":[]}`)
	write(t, filepath.Join(skills, "ops/sweep/SKILL.md"), "---\nname: sweep\n---\nRun sweep.py.\n")
	write(t, filepath.Join(skills, "ops/sweep/sweep.py"), "print('hi')\n")
	write(t, filepath.Join(skills, "ops/sweep/.proposal.json"), `{"skill":"ops/sweep","napHash":"h2","nap":"n","pii":[]}`)

	live := append([]manifest.Schedule{}, agent.Schedules...)
	live = append(live, manifest.Schedule{ID: "rt1", Name: "Digest", Cron: "0 8 * * *", Prompt: "Summarise yesterday's deals.", Agent: true, Enabled: true, Deliver: "slack"})
	wm, _ := LoadWatermark(root, "atlas")
	if ok, err := writeSchedules(proposalsRoot(root, "atlas"), live, agent.Schedules, nil, "n", wm, scrubber); err != nil || !ok {
		t.Fatalf("schedules proposal: %v %v", ok, err)
	}
	write(t, settingsPath(root, "atlas"), `[{"key":"home_channel.slack","value":"C0000000099","current":"C0000000001","opaque":true,"pii":[],"nap":"n"},
 {"key":"command_allowlist","value":"[\"rm -rf\"]","sensitive":true,"pii":[],"nap":"n"}]`)

	cands, held, err := ReviewCandidates(inst, "atlas", 1)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, c := range cands {
		got[c.Kind+":"+c.Ref] = true
		if c.Hash == "" || c.Body == "" {
			t.Errorf("candidate %s:%s has no hash or body", c.Kind, c.Ref)
		}
	}
	for _, want := range []string{"lesson:" + clean.ID, "skill:notes/brief", "schedules:schedules", "setting:home_channel.slack"} {
		if !got[want] {
			t.Errorf("missing candidate %s (got %v)", want, got)
		}
	}
	whyOf := map[string]string{}
	for _, h := range held {
		whyOf[h.Kind+":"+h.Ref] = h.Why
	}
	for ref, want := range map[string]string{
		"lesson:" + user.ID:         "about a person",
		"lesson:" + pii.ID:          "flagged",
		"skill:ops/sweep":           "sweep.py",
		"setting:command_allowlist": "widens",
	} {
		if !strings.Contains(whyOf[ref], want) {
			t.Errorf("%s held for %q, want %q", ref, whyOf[ref], want)
		}
	}

	t.Run("too few independent confirmations is held", func(t *testing.T) {
		_, held, _ := ReviewCandidates(inst, "atlas", 2)
		found := false
		for _, h := range held {
			found = found || h.Ref == clean.ID && strings.Contains(h.Why, "1 independent confirmations; need 2")
		}
		if !found {
			t.Error("a lesson seen once was not held at min_seen 2")
		}
	})

	t.Run("a schedule with a script or an explicit target is held", func(t *testing.T) {
		for _, s := range []manifest.Schedule{
			{ID: "rt2", Cron: "0 8 * * *", Script: "x.sh", Enabled: true},
			{ID: "rt3", Cron: "0 8 * * *", Prompt: "p", Agent: true, Deliver: "slack:C0SOMEWHERE", Enabled: true},
		} {
			_, why := schedulesCandidate(agent, append(append([]manifest.Schedule{}, agent.Schedules...), s), &SchedulesProposal{Hash: "x", PII: []string{}})
			if why == "" {
				t.Errorf("job %s was not held", s.ID)
			}
		}
		if _, why := schedulesCandidate(agent, nil, &SchedulesProposal{Hash: "x", PII: []string{}}); !strings.Contains(why, "removes") {
			t.Errorf("removing the repo's job: held for %q", why)
		}
	})

	t.Run("approving applies the decision and records who made it", func(t *testing.T) {
		by := Decider{By: "core review (test-model)", Reason: "plain operating fact"}
		for _, c := range cands {
			if err := Approve(inst, "atlas", c, by); err != nil {
				t.Fatalf("%s %s: %v", c.Kind, c.Ref, err)
			}
		}
		ledger, _ := learning.LoadLedger(root, "atlas")
		for _, e := range ledger {
			if e.ID == clean.ID && (e.Status != learning.Accepted || e.Note == nil || !strings.Contains(*e.Note, "core review (test-model)")) {
				t.Errorf("lesson = %+v", e)
			}
			if e.ID != clean.ID && e.Status != learning.Proposed {
				t.Errorf("held lesson %s decided", e.ID)
			}
		}
		if !fileExists(filepath.Join(root, "agents/atlas/skills/notes/brief/SKILL.md")) {
			t.Error("approved skill not copied into skills/")
		}
		a, _ := manifest.Load(root, "atlas", inst.Names.Secret)
		if len(a.Schedules) != 2 || a.Channels[0].HomeChannel != "C0000000099" {
			t.Errorf("agent.yaml schedules = %d, home = %q", len(a.Schedules), a.Channels[0].HomeChannel)
		}
		wm, _ := LoadWatermark(root, "atlas")
		if wm.SkillDecisions["notes/brief"].By == "" || wm.ScheduleDecision == nil || wm.ScheduleDecision.Reason != "plain operating fact" {
			t.Errorf("watermark = %+v", wm)
		}
		if err := Approve(inst, "atlas", cands[0], by); err == nil {
			t.Error("approving a decided proposal again succeeded")
		}
	})
}

func TestEvidenceReviewRevisionsAndNegativeReplay(t *testing.T) {
	inst := reviewInstance(t)
	root := inst.Root
	ledger := []*learning.Learning{}
	e, _ := learning.Upsert(&ledger, learning.Sighting{Kind: learning.KindMemory, Text: "Check the task assertion before claiming success.", Agent: "atlas", Unit: "sales", Nap: "n", At: "2026-10-26T10:00:00Z"})
	learning.SaveLedger(root, "atlas", ledger)
	cands, held, err := ReviewCandidates(inst, "atlas", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 || len(held) != 1 {
		t.Fatalf("unsupported note eligible %v %v", cands, held)
	}
	event := learning.OutcomeEvent{EventID: "check", TaskRoot: "task", Origin: "check", SubjectKind: "lesson", SubjectID: e.ID, SubjectRevision: learning.LessonRevision(e), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
	if err := learning.MergeEvidence(root, "atlas", []learning.OutcomeEvent{event}); err != nil {
		t.Fatal(err)
	}
	cands, _, err = ReviewCandidates(inst, "atlas", 1)
	if err != nil || len(cands) != 1 {
		t.Fatalf("confirmed note unavailable %v %v", cands, err)
	}
	old := cands[0]
	second := event
	second.EventID = "second"
	second.TaskRoot = "task2"
	second.Origin = "check2"
	learning.MergeEvidence(root, "atlas", []learning.OutcomeEvent{second})
	cands, _, _ = ReviewCandidates(inst, "atlas", 1)
	if cands[0].Hash == old.Hash {
		t.Fatal("evidence did not invalidate verdict hash")
	}
	negative := event
	negative.EventID = "negative"
	negative.TaskRoot = "failed-task"
	negative.Origin = "failed-check"
	negative.TaskOutcome = "failed"
	learning.MergeEvidence(root, "atlas", []learning.OutcomeEvent{negative, negative})
	cands, _, _ = ReviewCandidates(inst, "atlas", 1)
	if len(cands) != 0 {
		t.Fatal("negative evidence did not suspend auto eligibility")
	}
	if err := Approve(inst, "atlas", old, Decider{By: "test"}); err == nil {
		t.Fatal("cached approval accepted after contradiction")
	}
	e.Text = "Revised procedure."
	learning.SaveLedger(root, "atlas", ledger)
	cands, _, _ = ReviewCandidates(inst, "atlas", 1)
	if len(cands) != 0 {
		t.Fatal("revised text inherited confirmations")
	}
}

func TestApprovalRechecksConfirmationThreshold(t *testing.T) {
	inst := reviewInstance(t)
	ledger := []*learning.Learning{}
	e, _ := learning.Upsert(&ledger, learning.Sighting{Kind: learning.KindMemory, Text: "Check the quote currency before presenting totals.", Agent: "atlas", Unit: "sales", Nap: "n", At: "2026-10-26T10:00:00Z"})
	if err := learning.SaveLedger(inst.Root, "atlas", ledger); err != nil {
		t.Fatal(err)
	}
	event := learning.OutcomeEvent{EventID: "check", TaskRoot: "task", Origin: "check", SubjectKind: "lesson", SubjectID: e.ID, SubjectRevision: learning.LessonRevision(e), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
	if err := learning.MergeEvidence(inst.Root, "atlas", []learning.OutcomeEvent{event}); err != nil {
		t.Fatal(err)
	}
	inst.CoreLearning.AutoAcceptMinConfirmations = 1
	cands, _, err := ReviewCandidates(inst, "atlas", 1)
	if err != nil || len(cands) != 1 {
		t.Fatalf("candidate at threshold one: %v %v", cands, err)
	}
	inst.CoreLearning.AutoAcceptMinConfirmations = 2
	if err := Approve(inst, "atlas", cands[0], Decider{By: "test"}); err == nil {
		t.Fatal("approval applied after confirmation threshold increased")
	}
	after, err := learning.LoadLedger(inst.Root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if after[0].Status != learning.Proposed {
		t.Fatal("refused approval changed the decision")
	}
}
