package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"
)

// MaintenancePolicy is host-supplied policy, never model input. Ownership comes
// from runtime creation receipts; legacy skills without receipts remain protected.
type MaintenancePolicy struct {
	// Protected includes scheduled skill references and other owner-held skills,
	// resolved to directory names by the caller. Ancestors and children are held.
	Protected    []string
	StaleAfter   time.Duration
	ArchiveAfter time.Duration
}

// MaintenanceProposal describes a dry-run decision. Archive review is reversible
// intent only; this package provides no automatic archival or deletion operation.
type MaintenanceProposal struct {
	Dir          string    `json:"dir"`
	Action       string    `json:"action"`
	Reason       string    `json:"reason"`
	LastActivity time.Time `json:"last_activity,omitempty"`
	Reversible   bool      `json:"reversible,omitempty"`
}

// PlanMaintenance reads indexed skills and usage without changing files, counts,
// or visibility. Missing/malformed evidence holds skills rather than aging them
// from mtime. Viewing, using, creating and patching each reset inactivity.
func (l *Library) PlanMaintenance(p MaintenancePolicy) ([]MaintenanceProposal, error) {
	if p.StaleAfter <= 0 || p.ArchiveAfter <= p.StaleAfter {
		return nil, fmt.Errorf("maintenance requires positive stale age and a greater archive age")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	origins, err := l.readOrigins()
	if err != nil {
		return nil, err
	}
	// Repository protection must fail closed if the baseline receipt is unreadable.
	booted := map[string]string{}
	b, oversized, bootErr := l.readRegular(filepath.Join(l.Home, BootedFile), MaxFile)
	if !errors.Is(bootErr, fs.ErrNotExist) {
		var baseline struct {
			Skills map[string]string `json:"skills"`
		}
		if bootErr != nil || oversized || json.Unmarshal(b, &baseline) != nil || baseline.Skills == nil {
			return nil, fmt.Errorf("maintenance baseline evidence is invalid")
		}
		booted = baseline.Skills
	}
	// Unlike ordinary counting, planning must not silently repair corrupt usage.
	body, cut, err := l.readRegular(l.usagePath(), MaxFile)
	if err != nil || cut {
		return nil, fmt.Errorf("maintenance usage evidence unavailable or oversized: %v", err)
	}
	var usage map[string]usageEntry
	if err := json.Unmarshal(body, &usage); err != nil || usage == nil {
		return nil, fmt.Errorf("maintenance usage evidence is invalid")
	}
	idx := l.Index()
	names := map[string]int{}
	for _, s := range idx {
		names[s.Name]++
	}
	out := make([]MaintenanceProposal, 0, len(idx))
	now := l.now()
	for _, s := range idx {
		r := MaintenanceProposal{Dir: s.Dir, Action: "keep"}
		e := usage[s.Name]
		_, repositoryOwned := booted[s.Dir]
		switch {
		case l.ReadOnly(s.Dir):
			r.Reason = "generated or read-only"
		case repositoryOwned:
			r.Reason = "repository-owned"
		case maintenanceProtected(s.Dir, p.Protected):
			r.Reason = "protected reference"
		case names[s.Name] != 1:
			r.Reason = "ambiguous usage name"
		case e == nil:
			r.Reason = "missing usage evidence"
		case e["pinned"] != nil && e["pinned"] != false:
			r.Reason = "pinned"
		case !origins.owns(s, e) || e["created_by"] != "agent":
			r.Reason = "ownership not established"
		default:
			last, valid := maintenanceActivity(e, now)
			r.LastActivity = last
			if !valid {
				r.Reason = "missing or invalid activity evidence"
				break
			}
			age := now.Sub(last)
			switch {
			case age >= p.ArchiveAfter:
				r.Action, r.Reason, r.Reversible = "archive_review", "inactive agent-owned skill; review required", true
			case age >= p.StaleAfter:
				r.Action, r.Reason = "stale", "inactive agent-owned skill"
			default:
				r.Reason = "recent activity"
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func maintenanceProtected(dir string, protected []string) bool {
	for _, p := range protected {
		if p == "" || below(dir, p) || below(p, dir) {
			return true
		}
	}
	return false
}

func maintenanceActivity(e usageEntry, now time.Time) (time.Time, bool) {
	var last time.Time
	for _, key := range []string{"created_at", "last_used_at", "last_viewed_at", "last_patched_at"} {
		v := e[key]
		if v == nil {
			if key == "created_at" {
				return time.Time{}, false
			}
			continue
		}
		s, ok := v.(string)
		if !ok {
			return time.Time{}, false
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil || t.After(now) {
			return time.Time{}, false
		}
		if t.After(last) {
			last = t
		}
	}
	return last, !last.IsZero()
}
