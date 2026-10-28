package skills

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// DecisionsFile is private, bounded version 1 Raw state. Unknown versions are never rewritten.
const DecisionsFile = "state/needletail/skill-decisions-v1.json"

type PatchDecisionRequest struct {
	Root             string `json:"root"`
	Audience         string `json:"audience"`
	ID               string `json:"id"`
	Action           string `json:"action"`
	ExpectedRevision string `json:"expected_revision"`
}
type PatchReceipt struct {
	ID             string    `json:"id"`
	Root           string    `json:"root"`
	Audience       string    `json:"audience"`
	Dir            string    `json:"dir"`
	Status         string    `json:"status"`
	Before         string    `json:"before"`
	After          string    `json:"after"`
	BeforeRevision string    `json:"before_revision"`
	AfterRevision  string    `json:"after_revision"`
	UpdatedAt      time.Time `json:"updated_at"`
}
type PatchReview struct {
	After           string `json:"after"`
	CurrentRevision string `json:"current_revision"`
	SkillPatch
	Status  string        `json:"status"`
	Before  string        `json:"before"`
	Receipt *PatchReceipt `json:"receipt,omitempty"`
}
type decisionStore struct {
	Version  int            `json:"version"`
	Receipts []PatchReceipt `json:"receipts"`
}

func (l *Library) readPrivate(rel string, dst any) (bool, error) {
	b, cut, err := l.readRegular(filepath.Join(l.Home, rel), proposalBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if cut {
		return false, fmt.Errorf("private skill state oversized")
	}
	if err = json.Unmarshal(b, dst); err != nil {
		return false, err
	}
	return true, nil
}
func (l *Library) savePrivate(rel string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b)+1 > proposalBytes {
		return fmt.Errorf("private skill state full")
	}
	return fsx.WriteAtomicBelow(l.Home, rel, l.tmp(), append(b, '\n'), 0600)
}
func (l *Library) readDecisions() (decisionStore, error) {
	s := decisionStore{Version: 1, Receipts: []PatchReceipt{}}
	_, err := l.readPrivate(DecisionsFile, &s)
	if err != nil {
		return s, err
	}
	if s.Version != 1 || s.Receipts == nil || len(s.Receipts) > 256 {
		return s, fmt.Errorf("invalid skill decisions")
	}
	seen := map[string]bool{}
	for _, r := range s.Receipts {
		if r.ID == "" || seen[r.ID] || r.Root == "" || r.Audience == "" || r.Dir == "" || len(r.Before) > MaxFile || len(r.After) > MaxFile {
			return s, fmt.Errorf("invalid skill receipt")
		}
		seen[r.ID] = true
		switch r.Status {
		case "applying", "applied", "rejected", "rolling_back", "rolledback", "conflict":
		default:
			return s, fmt.Errorf("unknown receipt status")
		}
	}
	return s, nil
}
func trustedDecision(ctx context.Context) error {
	if why := messaging.UntrustedWhy(ctx); why != "" {
		return messaging.Deferred("owner skill decision", why)
	}
	return nil
}

// reconcile never replays a write. It classifies the live full bundle against the durable WAL.
func (l *Library) reconcile(s *decisionStore) error {
	changed := false
	for i := range s.Receipts {
		r := &s.Receipts[i]
		if r.Status != "applying" && r.Status != "rolling_back" {
			continue
		}
		skill, err := l.Find(r.Dir)
		if err != nil {
			return err
		}
		rev, err := l.bundleRevision(skill)
		if err != nil {
			return err
		}
		if r.Status == "applying" {
			switch rev {
			case r.AfterRevision:
				r.Status = "applied"
			case r.BeforeRevision:
				r.Status = "rolledback"
			default:
				r.Status = "conflict"
			}
		} else {
			switch rev {
			case r.BeforeRevision:
				r.Status = "rolledback"
			case r.AfterRevision:
				r.Status = "applied"
			default:
				r.Status = "conflict"
			}
		}
		changed = true
	}
	if changed {
		return l.savePrivate(DecisionsFile, s)
	}
	return nil
}

// DecidePatch requires an authenticated owner and sessions.Store.WithTrusted at the host boundary.
// Only agent-audience candidates may modify the globally executable skill directory.
func (l *Library) DecidePatch(ctx context.Context, q PatchDecisionRequest) (PatchReceipt, error) {
	if err := trustedDecision(ctx); err != nil {
		return PatchReceipt{}, err
	}
	if (q.Action == "approve" || q.Action == "rollback") && q.ExpectedRevision == "" {
		return PatchReceipt{}, fmt.Errorf("expected revision required")
	}
	if q.Root == "" || q.Audience == "" || q.ID == "" {
		return PatchReceipt{}, fmt.Errorf("exact scope required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ps, err := l.readPatches()
	if err != nil {
		return PatchReceipt{}, err
	}
	var p SkillPatch
	for _, v := range ps.Patches {
		if v.ID == q.ID && v.Root == q.Root && v.Audience == q.Audience {
			p = v
			break
		}
	}
	if p.ID == "" {
		return PatchReceipt{}, fmt.Errorf("proposal unavailable in scope")
	}
	ds, err := l.readDecisions()
	if err != nil {
		return PatchReceipt{}, err
	}
	if err = l.reconcile(&ds); err != nil {
		return PatchReceipt{}, err
	}
	at := -1
	for i, r := range ds.Receipts {
		if r.ID == p.ID {
			at = i
			break
		}
	}
	if q.Action == "reject" {
		if err := trustedDecision(ctx); err != nil {
			return PatchReceipt{}, err
		}
		if at >= 0 {
			r := ds.Receipts[at]
			if r.Status == "rejected" {
				return r, nil
			}
			return PatchReceipt{}, fmt.Errorf("proposal already decided")
		}
		r := PatchReceipt{ID: p.ID, Root: p.Root, Audience: p.Audience, Dir: p.Dir, Status: "rejected", Before: p.BaseContent, After: p.Content, BeforeRevision: p.ExpectedRevision, UpdatedAt: l.now().UTC()}
		if len(ds.Receipts) >= 256 {
			return PatchReceipt{}, fmt.Errorf("decision store full")
		}
		ds.Receipts = append(ds.Receipts, r)
		return r, l.savePrivate(DecisionsFile, ds)
	}
	if q.Audience != "agent" {
		return PatchReceipt{}, fmt.Errorf("private evidence cannot activate global skills")
	}
	s, err := l.Find(p.Dir)
	if err != nil {
		return PatchReceipt{}, err
	}
	if err = l.writable(s); err != nil {
		return PatchReceipt{}, err
	}
	rev, err := l.bundleRevision(s)
	if err != nil {
		return PatchReceipt{}, err
	}
	if q.Action == "rollback" {
		if at < 0 {
			return PatchReceipt{}, fmt.Errorf("no activation receipt")
		}
		r := ds.Receipts[at]
		if r.Status == "rolledback" {
			return r, nil
		}
		if r.Status != "applied" || q.ExpectedRevision != rev || rev != r.AfterRevision {
			return PatchReceipt{}, fmt.Errorf("rollback revision changed")
		}
		r.Status = "rolling_back"
		r.UpdatedAt = l.now().UTC()
		ds.Receipts[at] = r
		if err = l.savePrivate(DecisionsFile, ds); err != nil {
			return r, err
		}
		if err = trustedDecision(ctx); err != nil {
			return r, err
		}
		if err = l.writePreserving(s.Path, []byte(r.Before)); err != nil {
			return r, err
		}
		live, e := l.bundleRevision(s)
		if e != nil || live != r.BeforeRevision {
			return r, fmt.Errorf("rollback changed concurrently; reconcile receipt")
		}
		r.Status = "rolledback"
		ds.Receipts[at] = r
		return r, l.savePrivate(DecisionsFile, ds)
	}
	if q.Action != "approve" {
		return PatchReceipt{}, fmt.Errorf("unknown decision")
	}
	if at >= 0 {
		r := ds.Receipts[at]
		if r.Status == "applied" {
			if rev != r.AfterRevision {
				return PatchReceipt{}, fmt.Errorf("applied revision no longer live")
			}
			return r, nil
		}
		return PatchReceipt{}, fmt.Errorf("proposal already decided")
	}
	if q.ExpectedRevision != rev || rev != p.ExpectedRevision {
		return PatchReceipt{}, fmt.Errorf("skill bundle revision changed")
	}
	before, err := l.current(s)
	if err != nil {
		return PatchReceipt{}, err
	}
	if err = checkSkillMD([]byte(p.Content), before, path.Base(s.Dir), s.Name, s.Description != "", false); err != nil {
		return PatchReceipt{}, err
	}
	if err = l.check("SKILL.md", []byte(p.Content)); err != nil {
		return PatchReceipt{}, err
	}
	// Compute the future revision on an isolated copy, preserving every bundle byte and mode.
	afterRev, err := l.revisionWithText(s, p.Content)
	if err != nil {
		return PatchReceipt{}, err
	}
	current, err := l.bundleRevision(s)
	if err != nil || current != rev {
		return PatchReceipt{}, fmt.Errorf("skill changed during review")
	}
	r := PatchReceipt{ID: p.ID, Root: p.Root, Audience: p.Audience, Dir: p.Dir, Status: "applying", Before: string(before), After: p.Content, BeforeRevision: rev, AfterRevision: afterRev, UpdatedAt: l.now().UTC()}
	if len(ds.Receipts) >= 256 {
		return PatchReceipt{}, fmt.Errorf("decision store full")
	}
	ds.Receipts = append(ds.Receipts, r)
	if err = l.savePrivate(DecisionsFile, ds); err != nil {
		return r, err
	}
	if err = trustedDecision(ctx); err != nil {
		return r, err
	}
	if err = l.writePreserving(s.Path, []byte(p.Content)); err != nil {
		return r, err
	}
	live, e := l.bundleRevision(s)
	if e != nil || live != r.AfterRevision {
		return r, fmt.Errorf("activation changed concurrently; reconcile receipt")
	}
	r.Status = "applied"
	ds.Receipts[len(ds.Receipts)-1] = r
	l.patched(s)
	return r, l.savePrivate(DecisionsFile, ds)
}
func (l *Library) writePreserving(rel string, b []byte) error {
	f, err := fsx.OpenFileBelow(l.Home, rel, os.O_RDONLY, 0, false)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	f.Close()
	if err != nil {
		return err
	}
	return fsx.WriteAtomicBelow(l.Home, rel, l.tmp(), b, st.Mode().Perm())
}
func (l *Library) ReviewPatches(root, audience string) ([]PatchReview, error) {
	if root == "" || audience == "" {
		return nil, fmt.Errorf("scope required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ps, err := l.readPatches()
	if err != nil {
		return nil, err
	}
	ds, err := l.readDecisions()
	if err != nil {
		return nil, err
	}
	if err = l.reconcile(&ds); err != nil {
		return nil, err
	}
	out := []PatchReview{}
	for _, p := range ps.Patches {
		if p.Root != root || p.Audience != audience {
			continue
		}
		v := PatchReview{SkillPatch: p, Status: "pending", Before: p.BaseContent, After: p.Content}
		if s, e := l.Find(p.Dir); e == nil {
			v.CurrentRevision, _ = l.bundleRevision(s)
			b, e := l.current(s)
			if e == nil && v.Before == "" {
				v.Before = string(b)
			}
		}
		for _, r := range ds.Receipts {
			if r.ID == p.ID {
				r := r
				v.Receipt = &r
				v.Status = r.Status
				v.Before = r.Before
			}
		}
		out = append(out, v)
	}
	return out, nil
}
