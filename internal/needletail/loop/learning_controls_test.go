package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/learning"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func controlAgent(t *testing.T) (*Agent, learning.OutcomeEvent) {
	t.Helper()
	a := newAgent(t, &httptest.Server{URL: "http://127.0.0.1:1"})
	a.LLM = nil
	a.Cfg.BackgroundLearning = true
	a.Learning = &runtimelearn.Store{Home: a.Cfg.Home, Trusted: a.Store.WithTrusted}
	if _, err := a.Store.Create(wire.Session{ID: "source", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	e := learning.OutcomeEvent{EventID: runtimelearn.Hash("observed"), TaskRoot: runtimelearn.Hash("source"), SubjectKind: "skill", SubjectID: runtimelearn.Hash("procedure"), SubjectRevision: runtimelearn.Hash("revision"), At: a.now().Format(time.RFC3339), Audience: a.learningAudience("source"), SourceType: "runtime", Origin: runtimelearn.Hash("call"), ExecutionStatus: "failed", TaskOutcome: "unknown"}
	if err := a.Learning.Append(e); err != nil {
		t.Fatal(err)
	}
	return a, e
}
func controlLibrary(a *Agent) *skills.Library {
	l := &skills.Library{Home: a.Cfg.Home, Now: a.now}
	a.Env.Skills = l
	a.MaintenancePolicy = func() (skills.MaintenancePolicy, error) { return skills.MaintenancePolicy{}, nil }
	return l
}
func controlPatch(t *testing.T, a *Agent, aud string) skills.SkillPatch {
	t.Helper()
	l := a.Env.Skills
	rev, e := l.BundleRevision("crm-api")
	if e != nil {
		t.Fatal(e)
	}
	p, e := l.StagePatch(context.Background(), skills.PatchRequest{Root: runtimelearn.Hash("source"), Audience: aud, Name: "crm-api", ExpectedRevision: rev, OldText: "Query deals.", NewText: "Query current deals."})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestLearningControlsPatchScopePrivateApprovalAndRollback(t *testing.T) {
	a, _ := controlAgent(t)
	l := controlLibrary(a)
	if _, e := a.Store.Create(wire.Session{ID: "other", Source: "api_server"}); e != nil {
		t.Fatal(e)
	}
	private := controlPatch(t, a, a.learningAudience("source"))
	if _, e := a.LearningReview(context.Background(), "source", &LearningRequest{Action: "approve_patch", ID: private.ID, ExpectedRevision: private.ExpectedRevision}); e == nil {
		t.Fatal("private draft activated globally")
	}
	p := controlPatch(t, a, "agent")
	foreign, e := a.LearningReview(context.Background(), "other", nil)
	if e != nil {
		t.Fatal(e)
	}
	if foreign.Skills == nil || len(foreign.Skills.Patches) != 0 {
		t.Fatal("foreign patch listing")
	}
	req := &LearningRequest{Action: "approve_patch", ID: p.ID, ExpectedRevision: p.ExpectedRevision}
	if _, e = a.LearningReview(context.Background(), "other", req); e == nil {
		t.Fatal("foreign approval")
	}
	out, e := a.LearningReview(context.Background(), "source", req)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, v := range out.Skills.Patches {
		if v.ID == p.ID {
			found = v.Status == "applied" && v.CanRollback
		}
	}
	if !found {
		t.Fatal("activation receipt absent")
	}
	rev, e := l.BundleRevision("crm-api")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.LearningReview(context.Background(), "source", &LearningRequest{Action: "rollback_patch", ID: p.ID, ExpectedRevision: rev}); e != nil {
		t.Fatal(e)
	}
	if got, _ := l.BundleRevision("crm-api"); got != p.ExpectedRevision {
		t.Fatal("rollback did not restore bundle")
	}
	if e = a.Store.MarkUntrusted("source"); e != nil {
		t.Fatal(e)
	}
	if _, e = a.LearningReview(context.Background(), "source", &LearningRequest{Action: "reject_patch", ID: private.ID}); e == nil {
		t.Fatal("quarantined decision")
	}
}
func TestLearningControlsArchiveScopeAndLiveProtections(t *testing.T) {
	a, _ := controlAgent(t)
	l := controlLibrary(a)
	now := a.now()
	if _, e := l.Create("old", "", "---\nname: old\ndescription: Example.\n---\nExample."); e != nil {
		t.Fatal(e)
	}
	a.Now = func() time.Time { return now.Add(31 * 24 * time.Hour) }
	protected := true
	a.MaintenancePolicy = func() (skills.MaintenancePolicy, error) {
		p := skills.MaintenancePolicy{}
		if protected {
			p.Protected = []string{"old"}
		}
		return p, nil
	}
	if _, e := a.LearningReview(context.Background(), "source", &LearningRequest{Action: "scan_maintenance"}); e != nil {
		t.Fatal(e)
	}
	rev, _ := l.BundleRevision("old")
	q := &LearningRequest{Action: "archive_skill", ID: "old", ExpectedRevision: rev}
	if _, e := a.LearningReview(context.Background(), "source", q); e == nil {
		t.Fatal("scheduled protection bypass")
	}
	protected = false
	out, e := a.LearningReview(context.Background(), "source", &LearningRequest{Action: "scan_maintenance"})
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, v := range out.Skills.Maintenance {
		if v.Name == "old" {
			found = v.Action == "archive_review"
		}
	}
	if !found {
		t.Fatal("explicit scan retained stale protected cache")
	}
	out, e = a.LearningReview(context.Background(), "source", q)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Skills.Archives) != 1 {
		t.Fatal("archive receipt missing")
	}
	id := out.Skills.Archives[0].ID
	if _, e = a.Store.Create(wire.Session{ID: "other", Source: "api_server"}); e != nil {
		t.Fatal(e)
	}
	other, e := a.LearningReview(context.Background(), "other", nil)
	if e != nil || len(other.Skills.Archives) != 0 {
		t.Fatal("archive scope leak", e)
	}
	restore := &LearningRequest{Action: "restore_skill", ID: id}
	if _, e = a.LearningReview(context.Background(), "other", restore); e == nil {
		t.Fatal("foreign archive restored")
	}
	if _, e = a.LearningReview(context.Background(), "source", restore); e != nil {
		t.Fatal(e)
	}
	if got, _ := l.BundleRevision("old"); got != rev {
		t.Fatal("restore changed bundle")
	}
}
func TestLearningControlsRetryCancelPrunePreserveReservations(t *testing.T) {
	a, e := controlAgent(t)
	lib := controlLibrary(a)
	revision, _ := lib.BundleRevision("crm-api")
	patch, stageErr := lib.StagePatch(context.Background(), skills.PatchRequest{Root: e.TaskRoot, Audience: "agent", Name: "crm-api", ExpectedRevision: revision, OldText: "Query deals.", NewText: "Query verified deals.", SourceCandidate: runtimelearn.Hash("candidate"), EvidenceRefs: []string{e.EventID}, Checks: "Observed fixture failure", Conditions: "Same operation"})
	if stageErr != nil {
		t.Fatal(stageErr)
	}

	a.queueBackground("source", e, runtimelearn.TriggerToolFailure, "Observed failed execution")
	root, aud := e.TaskRoot, e.Audience
	jobs, err := a.Learning.BackgroundJobs(root, aud, a.now())
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	id := jobs[0].ID
	fail := func(context.Context, runtimelearn.BackgroundTrigger) (*runtimelearn.Candidate, error) {
		return nil, fmt.Errorf("provider response uncertain")
	}
	if _, err = a.Learning.ProcessBackground(context.Background(), a.now(), runtimelearn.BackgroundConfig{}, fail, nil); err == nil {
		t.Fatal("missing provider error")
	}
	if _, err = a.Store.Create(wire.Session{ID: "other", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	retry := &LearningRequest{Action: "retry_job", ID: id}
	if _, err = a.LearningReview(context.Background(), "other", retry); err == nil {
		t.Fatal("foreign retry")
	}
	out, err := a.LearningReview(context.Background(), "source", retry)
	if err != nil || out.Background.CallsUsed != 1 || out.Background.Jobs[0].Attempts != 1 {
		t.Fatal(out.Background, err)
	}
	out, err = a.LearningReview(context.Background(), "source", &LearningRequest{Action: "cancel_job", ID: id})
	if err != nil || out.Background.CallsUsed != 1 || out.Background.Jobs[0].State != "cancelled" {
		t.Fatal(out.Background, err)
	}
	// Model a retained terminal detail older than its current paid reservation.
	p := filepath.Join(a.Cfg.Home, runtimelearn.BackgroundFile)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var disk map[string]json.RawMessage
	if err = json.Unmarshal(b, &disk); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]json.RawMessage
	if err = json.Unmarshal(disk["jobs"], &rows); err != nil {
		t.Fatal(err)
	}
	rows[0]["updated"], _ = json.Marshal(a.now().Add(-31 * 24 * time.Hour))
	disk["jobs"], _ = json.Marshal(rows)
	b, _ = json.Marshal(disk)
	if err = os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	out, err = a.LearningReview(context.Background(), "source", &LearningRequest{Action: "prune_jobs"})
	if err != nil || len(out.Background.Jobs) != 0 || out.Background.CallsUsed != 1 || out.Background.TokensReserved != 512 {
		t.Fatal(out.Background, err)
	}
	retained, readErr := lib.ListPatches(e.TaskRoot, "agent")
	if readErr != nil || len(retained) != 1 || retained[0].ID != patch.ID || retained[0].Checks != patch.Checks || retained[0].Conditions != patch.Conditions || len(retained[0].EvidenceRefs) != 1 || retained[0].EvidenceRefs[0] != e.EventID {
		t.Fatal("queue prune erased independent patch evidence", retained, readErr)
	}
	a.queueBackground("source", e, runtimelearn.TriggerToolFailure, "Observed failed execution")
	out, err = a.LearningReview(context.Background(), "source", nil)
	if err != nil || len(out.Background.Jobs) != 0 || out.Background.CallsUsed != 1 {
		t.Fatal("prune lost dedupe", out.Background, err)
	}
}

// An export is checked by the dream's scrubber (the instance's rules, Env.Flag) as well as for
// secret values; the owner's feedback summary only for secret values.
func TestExportCheckUsesTheInstanceScrubber(t *testing.T) {
	a, _ := controlAgent(t)
	a.Env.Redact = func(s string) string { return strings.ReplaceAll(s, "fixture-secret", "[SECRET]") }
	a.Env.Flag = func(s string) []string {
		if strings.Contains(s, "AB12CD34") {
			return []string{"order-id"}
		}
		return nil
	}
	for text, refused := range map[string]bool{
		"Check the order total before replying.": false,
		"Order AB12CD34 needs a refund.":         true,
		"Use fixture-secret for the CRM.":        true,
	} {
		if err := a.exportCheck(text); (err != nil) != refused {
			t.Errorf("export %q: %v", text, err)
		}
	}
	if err := a.secretCheck("Order AB12CD34 needs a refund."); err != nil {
		t.Errorf("a feedback summary is checked for secrets only: %v", err)
	}
}
