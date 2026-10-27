package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// ProposalsFile is private Raw state, never skill/dream input. There is no apply API.
const ProposalsFile = "state/needletail/skill-proposals-v1.json"
const proposalBytes = 16 << 20

// PatchRequest scope comes from host-validated session provenance, never model arguments.
type PatchRequest struct {
	Root             string `json:"root"`
	Audience         string `json:"audience"`
	Name             string `json:"name"`
	ExpectedRevision string `json:"expected_revision"`
	OldText          string `json:"old_text"`
	NewText          string `json:"new_text"`
}

// SkillPatch is an immutable review candidate. ExpectedRevision binds the whole
// bundle at staging time; activation would need a fresh revision check and review.
type SkillPatch struct {
	ID               string    `json:"id"`
	Root             string    `json:"root"`
	Audience         string    `json:"audience"`
	Dir              string    `json:"dir"`
	ExpectedRevision string    `json:"expected_revision"`
	Content          string    `json:"content"`
	CreatedAt        time.Time `json:"created_at"`
}
type patchStore struct {
	Version int          `json:"version"`
	Patches []SkillPatch `json:"patches"`
}

func (l *Library) readPatches() (patchStore, error) {
	out := patchStore{Version: 1, Patches: []SkillPatch{}}
	b, cut, err := l.readRegular(filepath.Join(l.Home, ProposalsFile), proposalBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if cut || json.Unmarshal(b, &out) != nil || out.Version != 1 || out.Patches == nil || len(out.Patches) > 256 {
		return patchStore{}, fmt.Errorf("invalid skill proposal store")
	}
	for _, p := range out.Patches {
		if p.ID == "" || p.Root == "" || p.Audience == "" || p.Dir == "" || p.ExpectedRevision == "" || len(p.Content) > MaxFile {
			return patchStore{}, fmt.Errorf("invalid skill proposal")
		}
	}
	return out, nil
}

// BundleRevision hashes every path and regular-file content, including supporting
// files. Links, nonregular files, unreadable trees and oversized bundles are refused.
// Like staging, it is not a filesystem transaction against concurrent terminal writes.
func (l *Library) BundleRevision(name string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.Find(name)
	if err != nil {
		return "", err
	}
	return l.bundleRevision(s)
}
func (l *Library) bundleRevision(s Skill) (string, error) {
	h := sha256.New()
	h.Write([]byte("skill-bundle-v1\n"))
	total, files := 0, 0
	var walk func(string) error
	walk = func(rel string) error {
		fd, err := fsx.OpenDirBelow(l.Home, rel, false)
		if err != nil {
			return err
		}
		f := os.NewFile(uintptr(fd), rel)
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, d := range entries {
			p := path.Join(rel, d.Name())
			files++
			if files > 256 {
				return fmt.Errorf("skill bundle exceeds 256 entries")
			}
			fmt.Fprintf(h, "%d:%s:%d:", len(p), p, d.Type())
			if d.IsDir() {
				if err := walk(p); err != nil {
					return err
				}
				continue
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("skill bundle contains nonregular file %s", p)
			}
			f, err := fsx.OpenFileBelow(l.Home, p, os.O_RDONLY, 0, false)
			if err != nil {
				return err
			}
			info, statErr := f.Stat()
			b, readErr := io.ReadAll(io.LimitReader(f, MaxFile+1))
			f.Close()
			if statErr != nil {
				return statErr
			}
			if readErr != nil {
				return readErr
			}
			fmt.Fprintf(h, "%o:", info.Mode().Perm())
			total += len(b)
			if len(b) > MaxFile || total > 8<<20 {
				return fmt.Errorf("skill bundle exceeds size limit")
			}
			fmt.Fprintf(h, "%d:", len(b))
			h.Write(b)
		}
		return nil
	}
	if err := walk("skills/" + s.Dir); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// StagePatch validates a unique SKILL.md replacement and stores a review candidate
// without touching live skills, usage counts, tools or permissions. The host must
// authorize root/audience and serialize other writers if it needs a snapshot guarantee.
// A runtime caller must use sessions.Store.WithTrusted around this operation to
// serialize persistence against durable session quarantine. Context checks alone
// cannot provide that atomic authorization boundary.
func (l *Library) StagePatch(ctx context.Context, r PatchRequest) (SkillPatch, error) {
	if why := messaging.UntrustedWhy(ctx); why != "" {
		return SkillPatch{}, messaging.Deferred("staging a persistent skill proposal", why)
	}
	if strings.TrimSpace(r.Root) == "" || strings.TrimSpace(r.Audience) == "" || len(r.Root) > 256 || len(r.Audience) > 256 {
		return SkillPatch{}, fmt.Errorf("root and audience scope required")
	}
	if r.OldText == "" || r.ExpectedRevision == "" || len(r.OldText) > MaxFile || len(r.NewText) > MaxFile {
		return SkillPatch{}, fmt.Errorf("bounded old text and expected revision required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.Find(r.Name)
	if err != nil {
		return SkillPatch{}, err
	}
	if err = l.writable(s); err != nil {
		return SkillPatch{}, err
	}
	rev, err := l.bundleRevision(s)
	if err != nil {
		return SkillPatch{}, err
	}
	if rev != r.ExpectedRevision {
		return SkillPatch{}, fmt.Errorf("skill bundle revision changed")
	}
	b, cut, err := l.readRegular(filepath.Join(l.Home, s.Path), MaxFile)
	if err != nil {
		return SkillPatch{}, err
	}
	if cut {
		return SkillPatch{}, fmt.Errorf("SKILL.md exceeds limit")
	}
	old, repl := l.Unresolve(s, r.OldText), l.Unresolve(s, r.NewText)
	if strings.Count(string(b), old) != 1 {
		return SkillPatch{}, fmt.Errorf("old text must match exactly once")
	}
	content := strings.Replace(string(b), old, repl, 1)
	if len(content) > MaxFile {
		return SkillPatch{}, fmt.Errorf("proposal exceeds limit")
	}
	if err := checkSkillMD([]byte(content), b, path.Base(s.Dir), s.Name, s.Description != "", false); err != nil {
		return SkillPatch{}, err
	}
	if err := l.check("SKILL.md", []byte(content)); err != nil {
		return SkillPatch{}, err
	}
	p := SkillPatch{Root: r.Root, Audience: r.Audience, Dir: s.Dir, ExpectedRevision: rev, Content: content}
	identity, _ := json.Marshal(p)
	sum := sha256.Sum256(identity)
	p.ID = hex.EncodeToString(sum[:])
	p.CreatedAt = l.now().UTC()
	store, err := l.readPatches()
	if err != nil {
		return SkillPatch{}, err
	}
	for _, existing := range store.Patches {
		if existing.ID == p.ID {
			return existing, nil
		}
	}
	if len(store.Patches) >= 256 {
		return SkillPatch{}, fmt.Errorf("skill proposal store is full")
	}
	// Recheck after validation callbacks. This detects changes, but cannot lock out
	// arbitrary writers after this read; no activation is performed here.
	current, err := l.bundleRevision(s)
	if err != nil {
		return SkillPatch{}, err
	}
	if current != rev {
		return SkillPatch{}, fmt.Errorf("skill bundle changed while staging")
	}
	store.Patches = append(store.Patches, p)
	body, err := json.Marshal(store)
	if err != nil {
		return SkillPatch{}, err
	}
	if len(body)+1 > proposalBytes {
		return SkillPatch{}, fmt.Errorf("skill proposal store exceeds limit")
	}
	if why := messaging.UntrustedWhy(ctx); why != "" {
		return SkillPatch{}, messaging.Deferred("staging a persistent skill proposal", why)
	}
	if err := fsx.WriteAtomicBelow(l.Home, ProposalsFile, l.tmp(), append(body, '\n'), 0600); err != nil {
		return SkillPatch{}, err
	}
	return p, nil
}

// ListPatches exposes only exact host-authorized scope matches.
func (l *Library) ListPatches(root, audience string) ([]SkillPatch, error) {
	if root == "" || audience == "" {
		return nil, fmt.Errorf("root and audience scope required")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	store, err := l.readPatches()
	if err != nil {
		return nil, err
	}
	out := []SkillPatch{}
	for _, p := range store.Patches {
		if p.Root == root && p.Audience == audience {
			out = append(out, p)
		}
	}
	return out, nil
}
