package loop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/pkg/learning"
)

func reviewAudiences(aud string) []string {
	if aud == "agent" {
		return []string{aud}
	}
	return []string{aud, "agent"}
}

func (a *Agent) skillMaintenancePolicy() (skills.MaintenancePolicy, error) {
	if a.MaintenancePolicy == nil {
		return skills.MaintenancePolicy{}, fmt.Errorf("scheduled skill protections unavailable")
	}
	return a.MaintenancePolicy()
}

// RunSkillMaintenance owns a daily, no-model recommendation scan. It never
// archives anything automatically; archive requests recheck current protections.
func (a *Agent) RunSkillMaintenance(ctx context.Context) {
	if a.Cfg == nil || !a.Cfg.SkillMaintenance || a.Env.Skills == nil {
		return
	}
	scan := func() {
		if a.activeTurns.Load() != 0 {
			return
		}
		policy, err := a.skillMaintenancePolicy()
		if err == nil {
			_, err = a.Env.Skills.ScanMaintenance(policy)
		}
		if err != nil {
			a.logf("skill maintenance: scan refused; review current protections")
		}
	}
	scan()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scan()
		}
	}
}

func (a *Agent) learningControl(ctx context.Context, session, root, aud string, r *LearningRequest) error {
	if !r.Valid() {
		return fmt.Errorf("invalid learning operation")
	}
	if r.Action == "cancel_job" || r.Action == "retry_job" {
		for _, scope := range reviewAudiences(aud) {
			jobs, err := a.Learning.BackgroundJobs(root, scope, a.now())
			if err != nil {
				return err
			}
			for _, job := range jobs {
				if job.ID != r.ID {
					continue
				}
				if r.Action == "retry_job" {
					if !a.Cfg.BackgroundLearning {
						return fmt.Errorf("background learning is disabled")
					}
					// Retry rechecks the original source with its own trust guard.
					return a.Learning.RetryBackground(root, scope, r.ID, a.now())
				}
				return a.Store.WithTrusted(session, func() error { return a.Learning.CancelBackground(root, scope, r.ID) })
			}
		}
		return fmt.Errorf("background job not found in this scope")
	}
	return a.Store.WithTrusted(session, func() error {
		if r.Action == "prune_jobs" {
			for _, scope := range reviewAudiences(aud) {
				if _, err := a.Learning.PruneBackground(root, scope, a.now()); err != nil {
					return err
				}
			}
			return nil
		}
		lib := a.Env.Skills
		if lib == nil {
			return fmt.Errorf("skills unavailable")
		}
		switch r.Action {
		case "approve_patch", "reject_patch", "rollback_patch":
			for _, scope := range reviewAudiences(aud) {
				patches, err := lib.ListPatches(root, scope)
				if err != nil {
					return err
				}
				for _, p := range patches {
					if p.ID == r.ID {
						_, err := lib.DecidePatch(ctx, skills.PatchDecisionRequest{Root: root, Audience: scope, ID: r.ID, Action: strings.TrimSuffix(r.Action, "_patch"), ExpectedRevision: r.ExpectedRevision})
						return err
					}
				}
			}
			return fmt.Errorf("skill proposal not found in this scope")
		case "scan_maintenance":
			policy, err := a.skillMaintenancePolicy()
			if err != nil {
				return err
			}
			_, err = lib.ForceScanMaintenance(policy)
			return err
		case "archive_skill":
			policy, err := a.skillMaintenancePolicy()
			if err != nil {
				return err
			}
			_, err = lib.Archive(ctx, skills.ArchiveRequest{Root: root, Audience: "agent", Name: r.ID, ExpectedRevision: r.ExpectedRevision}, policy)
			return err
		case "restore_skill":
			_, err := lib.ScopedRestoreArchive(ctx, root, "agent", r.ID)
			return err
		}
		return fmt.Errorf("unknown learning action")
	})
}

func reviewTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (a *Agent) backgroundJobs(root, aud string) ([]learning.BackgroundJobView, error) {
	result := []learning.BackgroundJobView{}
	for _, scope := range reviewAudiences(aud) {
		jobs, err := a.Learning.BackgroundJobs(root, scope, a.now())
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			result = append(result, learning.BackgroundJobView{ID: j.ID, Kind: string(j.Kind), State: j.State, Attempts: j.Attempts, OwnerRetries: j.OwnerRetries, CreatedAt: reviewTime(j.CreatedAt), UpdatedAt: reviewTime(j.UpdatedAt), Next: reviewTime(j.Next), ResultCode: j.ResultCode, CandidateID: j.CandidateID})
		}
	}
	return result, nil
}

func (a *Agent) skillReview(root, aud string, trusted bool) (*learning.SkillReviewData, error) {
	lib := a.Env.Skills
	if lib == nil {
		return nil, nil
	}
	out := &learning.SkillReviewData{Patches: []learning.SkillPatchView{}, Maintenance: []learning.SkillMaintenanceView{}, Archives: []learning.SkillArchiveView{}, MaintenanceEnabled: a.Cfg.SkillMaintenance}
	for _, scope := range reviewAudiences(aud) {
		patches, err := lib.ReviewPatches(root, scope)
		if err != nil {
			return nil, err
		}
		for _, p := range patches {
			v := learning.SkillPatchView{ID: p.ID, Name: p.Dir, Status: p.Status, Before: p.Before, After: p.After, ExpectedRevision: p.ExpectedRevision, CurrentRevision: p.CurrentRevision, Audience: p.Audience, CreatedAt: reviewTime(p.CreatedAt)}
			v.EvidenceRefs, v.Checks, v.Conditions = p.EvidenceRefs, p.Checks, p.Conditions
			v.CanApprove = trusted && p.Audience == "agent" && p.Status == "pending" && p.CurrentRevision != "" && p.CurrentRevision == p.ExpectedRevision
			v.CanRollback = trusted && p.Audience == "agent" && p.Status == "applied" && p.Receipt != nil && p.CurrentRevision == p.Receipt.AfterRevision
			if p.Audience != "agent" {
				v.Reason = "Private source: cannot activate a globally available skill."
			} else if p.Status == "pending" && p.CurrentRevision != p.ExpectedRevision {
				v.Reason = "The skill changed since this draft. Generate a new draft before approval."
			}
			out.Patches = append(out.Patches, v)
		}
	}
	scan, err := lib.MaintenanceSnapshot()
	if err != nil {
		return nil, err
	}
	out.LastScanAt = reviewTime(scan.ScannedAt)
	for _, m := range scan.Proposals {
		revision, _ := lib.BundleRevision(m.Dir)
		out.Maintenance = append(out.Maintenance, learning.SkillMaintenanceView{Name: m.Dir, Action: m.Action, Reason: m.Reason, Revision: revision, LastActivity: reviewTime(m.LastActivity)})
	}
	archives, err := lib.ListArchives()
	if err != nil {
		return nil, err
	}
	for _, v := range archives {
		if v.Root == root && v.Audience == "agent" {
			out.Archives = append(out.Archives, learning.SkillArchiveView{ID: v.ID, Name: v.Dir, Status: v.Status, Revision: v.Revision, CreatedAt: reviewTime(v.CreatedAt)})
		}
	}
	return out, nil
}
