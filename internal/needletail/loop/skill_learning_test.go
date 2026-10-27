package loop

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/learning"
)

const skillLearningOriginal = "---\nname: crm-api\ndescription: Query deals.\n---\nCheck the response.\n"
const skillLearningResponse = `{"text":"Check the documented response before declaring success.","checks":"Owner corrected the procedure.","conditions":"When running this check.","sensitivity":"procedural","reviewDate":"2026-11-20","action":"new","relatedIds":[],"skillDraft":{"oldText":"Check the response.","newText":"Check the response and its status."}}`

func skillLearningAgent(t *testing.T, private bool, response string, during func(*Agent)) (*Agent, *stub, learning.OutcomeEvent) {
	t.Helper()
	var a *Agent
	st := &stub{pick: func(req map[string]any) string {
		if req["max_tokens"] != float64(512) || req["tools"] != nil {
			t.Errorf("unbounded/tool request: %+v", req)
		}
		if during != nil {
			during(a)
		}
		return say(response)
	}}
	srv := httptest.NewServer(st)
	t.Cleanup(srv.Close)
	a = newAgent(t, srv)
	a.Cfg.BackgroundLearning = true
	if !private {
		a.Env.Memory.Where = func(string) memory.Audience { return memory.Audience{Operator: true} }
	}
	a.Env.Skills = &skills.Library{Home: a.Cfg.Home, Now: a.Now}
	p := filepath.Join(a.Cfg.Home, "skills/crm/crm-api/SKILL.md")
	if err := os.WriteFile(p, []byte(skillLearningOriginal), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.Create(wire.Session{ID: "skill-source", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	a.Learning = &runtimelearn.Store{Home: a.Cfg.Home, Trusted: a.Store.WithTrusted}
	skill, err := a.Env.Skills.Find("crm-api")
	if err != nil {
		t.Fatal(err)
	}
	e := learning.OutcomeEvent{EventID: runtimelearn.Hash("skill-view"), TaskRoot: runtimelearn.Hash("skill-source"), SubjectKind: "skill", SubjectID: runtimelearn.Hash(skill.Dir), SubjectRevision: func() string {
		revision, err := a.Env.Skills.BundleRevision(skill.Dir)
		if err != nil {
			t.Fatal(err)
		}
		return revision
	}(), At: a.now().Format("2006-01-02T15:04:05Z"), Audience: a.learningAudience("skill-source"), SourceType: "view", Origin: runtimelearn.Hash("skill-call"), ExecutionStatus: "unknown", TaskOutcome: "unknown"}
	if err := a.Learning.Append(e); err != nil {
		t.Fatal(err)
	}
	if _, err := a.LearningReview(context.Background(), "skill-source", &LearningRequest{EventID: e.EventID, Outcome: "corrected", Origin: "owner-correction", Summary: "Owner verified the response status must be checked."}); err != nil {
		t.Fatal(err)
	}
	return a, st, e
}
func processSkillLearning(a *Agent) (bool, error) {
	return a.Learning.ProcessBackgroundWithResult(context.Background(), a.now(), runtimelearn.BackgroundConfig{}, a.reflectWithSkill, tools.SecretCheck(a.Env.Redact), a.stageBackgroundSkill)
}
func assertSkillBytes(t *testing.T, a *Agent, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.Cfg.Home, "skills/crm/crm-api/SKILL.md"))
	if err != nil || string(b) != want {
		t.Fatal(string(b), err)
	}
}

func TestSkillLearningCorrectionStagesSamePaidCallAndReviewLifecycle(t *testing.T) {
	a, st, e := skillLearningAgent(t, false, skillLearningResponse, nil)
	if worked, err := processSkillLearning(a); !worked || err != nil {
		t.Fatal(worked, err)
	}
	st.mu.Lock()
	n := len(st.requests)
	input, _ := json.Marshal(st.requests)
	st.mu.Unlock()
	if n != 1 || !strings.Contains(string(input), "Check the response.") {
		t.Fatal(n, string(input))
	}
	assertSkillBytes(t, a, skillLearningOriginal)
	out, err := a.LearningReview(context.Background(), "skill-source", nil)
	if err != nil || out.Skills == nil || len(out.Skills.Patches) != 1 || len(out.Background.Candidates) != 1 || out.Background.CallsUsed != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	p := out.Skills.Patches[0]
	if !p.CanApprove || p.CanRollback || p.Audience != "agent" {
		t.Fatal(p)
	}
	if out.Background.Candidates[0].SkillDraft.Name != "crm/crm-api" {
		t.Fatal("model target not host bound")
	}
	if _, err := a.LearningReview(context.Background(), "skill-source", &LearningRequest{Action: "approve_patch", ID: p.ID, ExpectedRevision: p.ExpectedRevision}); err != nil {
		t.Fatal(err)
	}
	assertSkillBytes(t, a, strings.ReplaceAll(skillLearningOriginal, "Check the response.", "Check the response and its status."))
	out, err = a.LearningReview(context.Background(), "skill-source", nil)
	if err != nil || !out.Skills.Patches[0].CanRollback {
		t.Fatal(out.Skills, err)
	}
	current, err := a.Env.Skills.BundleRevision("crm-api")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.LearningReview(context.Background(), "skill-source", &LearningRequest{Action: "rollback_patch", ID: p.ID, ExpectedRevision: current}); err != nil {
		t.Fatal(err)
	}
	assertSkillBytes(t, a, skillLearningOriginal)
	patches, err := a.Env.Skills.ListPatches(e.TaskRoot, e.Audience)
	if err != nil || len(patches) != 1 {
		t.Fatal(patches, err)
	}
}

func TestSkillLearningPrivateEvidenceCannotDraftGlobalSkill(t *testing.T) {
	a, st, e := skillLearningAgent(t, true, skillLearningResponse, nil)
	if _, err := processSkillLearning(a); err == nil {
		t.Fatal("generic private reflector accepted skill draft")
	}
	patches, err := a.Env.Skills.ListPatches(e.TaskRoot, "agent")
	if err != nil || len(patches) != 0 {
		t.Fatal(patches, err)
	}
	st.mu.Lock()
	input, _ := json.Marshal(st.requests)
	n := len(st.requests)
	st.mu.Unlock()
	if n != 1 || strings.Contains(string(input), "Check the response.") {
		t.Fatal("private reflector received skill context", string(input))
	}
	assertSkillBytes(t, a, skillLearningOriginal)
}

func TestSkillLearningRefusesRevisionChangeDuringModel(t *testing.T) {
	changed := skillLearningOriginal + "Owner's newer edit.\n"
	a, _, e := skillLearningAgent(t, false, skillLearningResponse, func(a *Agent) {
		if err := os.WriteFile(filepath.Join(a.Cfg.Home, "skills/crm/crm-api/SKILL.md"), []byte(changed), 0644); err != nil {
			t.Error(err)
		}
	})
	if _, err := processSkillLearning(a); err == nil {
		t.Fatal("stale draft staged")
	}
	patches, err := a.Env.Skills.ListPatches(e.TaskRoot, e.Audience)
	if err != nil || len(patches) != 0 {
		t.Fatal(patches, err)
	}
	assertSkillBytes(t, a, changed)
}

func TestSkillLearningCancellationSuppressesPatchStaging(t *testing.T) {
	a, _, e := skillLearningAgent(t, false, skillLearningResponse, func(a *Agent) {
		jobs, err := a.Learning.BackgroundJobs(runtimelearn.Hash("skill-source"), "agent", a.now())
		if err != nil || len(jobs) != 1 {
			t.Error(err)
			return
		}
		if err := a.Learning.CancelBackground(runtimelearn.Hash("skill-source"), "agent", jobs[0].ID); err != nil {
			t.Error(err)
		}
	})
	if _, err := processSkillLearning(a); err == nil {
		t.Fatal("cancelled reservation accepted")
	}
	patches, err := a.Env.Skills.ListPatches(e.TaskRoot, e.Audience)
	if err != nil || len(patches) != 0 {
		t.Fatal(patches, err)
	}
	assertSkillBytes(t, a, skillLearningOriginal)
}

func TestSkillLearningMissingObservedRevisionCannotInventTarget(t *testing.T) {
	a, st, e := skillLearningAgent(t, false, skillLearningResponse, nil)
	// The observed bundle revision is stale; a generic response must not gain
	// authority by supplying an arbitrary current revision itself.
	if err := os.WriteFile(filepath.Join(a.Cfg.Home, "skills/crm/crm-api/reference.md"), []byte("Owner added a reference."), 0644); err != nil {
		t.Fatal(err)
	}
	revision, err := a.Env.Skills.BundleRevision("crm-api")
	if err != nil {
		t.Fatal(err)
	}
	response := strings.Replace(skillLearningResponse, `"skillDraft":{`, `"skillDraft":{"name":"crm/crm-api","expectedRevision":"`+revision+`",`, 1)
	st.pick = func(map[string]any) string { return say(response) }
	if _, err := processSkillLearning(a); err == nil {
		t.Fatal("generic model invented a current skill target")
	}
	patches, err := a.Env.Skills.ListPatches(e.TaskRoot, e.Audience)
	if err != nil || len(patches) != 0 {
		t.Fatal(patches, err)
	}
	assertSkillBytes(t, a, skillLearningOriginal)
}

func TestSkillLearningPermissionChangeInvalidatesObservation(t *testing.T) {
	a, st, e := skillLearningAgent(t, false, "null", nil)
	if err := os.Chmod(filepath.Join(a.Cfg.Home, "skills/crm/crm-api/SKILL.md"), 0755); err != nil {
		t.Fatal(err)
	}
	if worked, err := processSkillLearning(a); !worked || err != nil {
		t.Fatal(worked, err)
	}
	st.mu.Lock()
	input, _ := json.Marshal(st.requests)
	st.mu.Unlock()
	if strings.Contains(string(input), "Check the response.") {
		t.Fatal("mode change failed to invalidate observed bundle", string(input))
	}
	patches, err := a.Env.Skills.ListPatches(e.TaskRoot, e.Audience)
	if err != nil || len(patches) != 0 {
		t.Fatal(patches, err)
	}
}

func TestSkillLearningNewObservationsUseCanonicalBundleRevision(t *testing.T) {
	a, _, _ := skillLearningAgent(t, false, "null", nil)
	call := wire.ToolCall{Name: "skill_view", Arguments: `{"name":"crm-api"}`}
	before := a.outcomeSubject(call)
	canonical, err := a.Env.Skills.BundleRevision("crm-api")
	if err != nil || before == nil || before.SubjectRevision != canonical {
		t.Fatal(before, canonical, err)
	}
	if err := os.Chmod(filepath.Join(a.Cfg.Home, "skills/crm/crm-api/SKILL.md"), 0755); err != nil {
		t.Fatal(err)
	}
	after := a.outcomeSubject(call)
	if after == nil || after.SubjectRevision == before.SubjectRevision {
		t.Fatal("mode change omitted from new attribution", before, after)
	}
}
