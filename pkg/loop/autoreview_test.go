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
	// A Hermes agent records no outcome evidence: what the model reads and the hash its verdict is
	// kept under are what they were before evidence existed (cached verdicts stay valid).
	for _, c := range cands {
		switch c.Kind + ":" + c.Ref {
		case "lesson:" + clean.ID:
			if c.Hash != clean.ID || c.Body != "A lesson the agent wrote into its own memory (seen 1 times):\n\n"+clean.Text {
				t.Errorf("Hermes lesson candidate changed: hash %q body %q", c.Hash, c.Body)
			}
		case "skill:notes/brief":
			if c.Hash != "h1" || strings.Contains(c.Body, "evidence") {
				t.Errorf("Hermes skill candidate changed: hash %q body %q", c.Hash, c.Body)
			}
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

	t.Run("seen too few times is held", func(t *testing.T) {
		_, held, _ := ReviewCandidates(inst, "atlas", 2)
		found := false
		for _, h := range held {
			found = found || h.Ref == clean.ID && strings.Contains(h.Why, "seen 1 of 2")
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

// Outcome evidence is what the model reads beside a change, never a rule: a lesson seen enough
// times reaches the model with or without confirmations, and one with recorded failures still
// reaches it, marked as such. Its verdict is keyed to the evidence, so new evidence asks again.
func TestEvidenceIsReviewInputNotGate(t *testing.T) {
	withLearningEngine(t)
	inst := reviewInstance(t)
	root := inst.Root
	ledger := []*learning.Learning{}
	e, _ := learning.Upsert(&ledger, learning.Sighting{Kind: learning.KindMemory, Text: "Check the task assertion before claiming success.", Agent: "atlas", Unit: "sales", Nap: "n", At: "2026-10-26T10:00:00Z"})
	if err := learning.SaveLedger(root, "atlas", ledger); err != nil {
		t.Fatal(err)
	}
	only := func(min int) Candidate {
		t.Helper()
		cands, held, err := ReviewCandidates(inst, "atlas", min)
		if err != nil || len(cands) != 1 || len(held) != 0 {
			t.Fatalf("candidates %v held %v: %v", cands, held, err)
		}
		return cands[0]
	}
	none := only(1)
	if !strings.Contains(none.Evidence, "No outcome evidence") || strings.Contains(none.Body, "evidence") || none.Hash == e.ID {
		t.Fatalf("an engine that records outcomes: evidence %q body %q hash %q", none.Evidence, none.Body, none.Hash)
	}

	event := learning.OutcomeEvent{EventID: "check", TaskRoot: "task", Origin: "check", SubjectKind: "lesson", SubjectID: e.ID, SubjectRevision: learning.LessonRevision(e), At: "2026-10-26T10:00:00Z", Audience: "agent", SourceType: "assertion", ExecutionStatus: "succeeded", TaskOutcome: "succeeded"}
	if err := learning.MergeEvidence(root, "atlas", []learning.OutcomeEvent{event}); err != nil {
		t.Fatal(err)
	}
	confirmed := only(1)
	if confirmed.Hash == none.Hash || !strings.Contains(confirmed.Evidence, "Independent confirmations (a task check or authorized feedback found the result right): 1") || strings.Contains(confirmed.Evidence, "WARNING") {
		t.Fatalf("confirmed: %+v", confirmed)
	}

	negative := event
	negative.EventID, negative.TaskRoot, negative.Origin, negative.TaskOutcome = "negative", "failed-task", "failed-check", "failed"
	if err := learning.MergeEvidence(root, "atlas", []learning.OutcomeEvent{negative, negative}); err != nil {
		t.Fatal(err)
	}
	failed := only(1)
	if failed.Hash == confirmed.Hash || !strings.HasPrefix(failed.Evidence, "WARNING: this exact version has 1 recorded failure(s) and 0 correction(s)") {
		t.Fatalf("a recorded failure is not presented as one: %+v", failed)
	}
	if err := Approve(inst, "atlas", confirmed, Decider{By: "test"}); err == nil {
		t.Fatal("a verdict given before the failure was recorded was applied")
	}

	// The seen-count rule still decides what reaches the model; confirmations do not bypass it.
	if _, held, _ := ReviewCandidates(inst, "atlas", 2); len(held) != 1 || !strings.Contains(held[0].Why, "seen 1 of 2") {
		t.Fatalf("held at min_seen 2: %v", held)
	}

	e.Text = "Revised procedure."
	if err := learning.SaveLedger(root, "atlas", ledger); err != nil {
		t.Fatal(err)
	}
	if revised := only(1); !strings.Contains(revised.Evidence, "No outcome evidence") {
		t.Fatalf("revised text inherited the old version's evidence: %q", revised.Evidence)
	}
	if err := Approve(inst, "atlas", failed, Decider{By: "test"}); err == nil {
		t.Fatal("a verdict on the old text was applied to the revised one")
	}
}

// An unreadable evidence store fails nothing: the review goes on, and the model is told the
// evidence could not be read rather than that none was recorded.
func TestUnreadableEvidenceDoesNotStopReview(t *testing.T) {
	withLearningEngine(t)
	inst := reviewInstance(t)
	root := inst.Root
	ledger := []*learning.Learning{}
	learning.Upsert(&ledger, learning.Sighting{Kind: learning.KindMemory, Text: "Reports are due on Mondays.", Agent: "atlas", Unit: "sales", Nap: "n", At: "2026-10-26T10:00:00Z"})
	if err := learning.SaveLedger(root, "atlas", ledger); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(ProposalsDir(root, "atlas"), "notes/brief/SKILL.md"), "---\nname: brief\n---\nKeep briefs to five lines.\n")
	write(t, filepath.Join(ProposalsDir(root, "atlas"), "notes/brief/.proposal.json"), `{"skill":"notes/brief","napHash":"h1","nap":"n","pii":[]}`)
	write(t, learning.EvidencePath(root, "atlas"), "{not json")
	cands, _, err := ReviewCandidates(inst, "atlas", 1)
	if err != nil || len(cands) != 2 {
		t.Fatalf("candidates %v: %v", cands, err)
	}
	for _, c := range cands {
		if !strings.Contains(c.Evidence, "could not be read") || strings.Contains(c.Evidence, "No outcome evidence is recorded") {
			t.Errorf("%s %s evidence: %q", c.Kind, c.Ref, c.Evidence)
		}
	}
}
