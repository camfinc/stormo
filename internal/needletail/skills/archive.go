package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/camfinc/stormo/internal/needletail/fsx"
	"golang.org/x/sys/unix"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const MaintenanceFile = "state/needletail/skill-maintenance-v1.json"
const ArchivesFile = "state/needletail/skill-archives-v1.json"
const ArchiveDir = "state/needletail/skill-archives-v1"

type MaintenanceScan struct {
	Version   int                   `json:"version"`
	ScannedAt time.Time             `json:"scanned_at"`
	NextDue   time.Time             `json:"next_due"`
	Proposals []MaintenanceProposal `json:"proposals"`
}

// ScanMaintenance persists a daily recommendation only. It never removes a skill.
func (l *Library) ScanMaintenance(p MaintenancePolicy) (MaintenanceScan, error) {
	return l.scanMaintenance(p, false)
}
func (l *Library) ForceScanMaintenance(p MaintenancePolicy) (MaintenanceScan, error) {
	return l.scanMaintenance(p, true)
}
func (l *Library) MaintenanceSnapshot() (MaintenanceScan, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := MaintenanceScan{}
	exists, err := l.readPrivate(MaintenanceFile, &s)
	if err != nil {
		return s, err
	}
	if exists && (s.Version != 1 || s.Proposals == nil || len(s.Proposals) > 4096) {
		return s, fmt.Errorf("invalid maintenance state")
	}
	return s, nil
}
func (l *Library) scanMaintenance(p MaintenancePolicy, force bool) (MaintenanceScan, error) {
	l.mu.Lock()
	s := MaintenanceScan{}
	exists, err := l.readPrivate(MaintenanceFile, &s)
	if err != nil {
		l.mu.Unlock()
		return s, err
	}
	if exists && (s.Version != 1 || s.Proposals == nil || len(s.Proposals) > 4096 || s.ScannedAt.After(l.now()) || !s.NextDue.After(s.ScannedAt)) {
		l.mu.Unlock()
		return s, fmt.Errorf("invalid maintenance state")
	}
	if exists && !force && l.now().Before(s.NextDue) {
		l.mu.Unlock()
		return s, nil
	}
	l.mu.Unlock()
	if p.StaleAfter == 0 {
		p.StaleAfter = 14 * 24 * time.Hour
	}
	if p.ArchiveAfter == 0 {
		p.ArchiveAfter = 30 * 24 * time.Hour
	}
	v, err := l.PlanMaintenance(p)
	if err != nil {
		return s, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(v) > 4096 {
		return s, fmt.Errorf("maintenance inventory full")
	}
	s = MaintenanceScan{Version: 1, ScannedAt: l.now().UTC(), NextDue: l.now().Add(24 * time.Hour).UTC(), Proposals: v}
	return s, l.savePrivate(MaintenanceFile, s)
}

type ArchiveRequest struct {
	Root             string `json:"root"`
	Audience         string `json:"audience"`
	Name             string `json:"name"`
	ExpectedRevision string `json:"expected_revision"`
}
type ArchiveReceipt struct {
	Modes     map[string]uint32 `json:"modes"`
	Root      string            `json:"root"`
	Audience  string            `json:"audience"`
	ID        string            `json:"id"`
	Dir       string            `json:"dir"`
	Revision  string            `json:"revision"`
	Status    string            `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
}
type archiveStore struct {
	Version  int              `json:"version"`
	Archives []ArchiveReceipt `json:"archives"`
}

func (l *Library) readArchives() (archiveStore, error) {
	s := archiveStore{Version: 1, Archives: []ArchiveReceipt{}}
	_, err := l.readPrivate(ArchivesFile, &s)
	if err != nil {
		return s, err
	}
	if s.Version != 1 || s.Archives == nil || len(s.Archives) > 256 {
		return s, fmt.Errorf("invalid archive state")
	}
	seen := map[string]bool{}
	for _, r := range s.Archives {
		dir, e := cleanRel(r.Dir)
		id, e2 := hex.DecodeString(r.ID)
		if r.Root == "" || r.Audience != "agent" || e != nil || dir != r.Dir || e2 != nil || len(id) != 32 || seen[r.ID] || r.Revision == "" || r.Modes == nil || len(r.Modes) > 256 {
			return s, fmt.Errorf("invalid archive receipt")
		}
		for p, m := range r.Modes {
			clean, e := cleanRel(p)
			if e != nil || clean != p || m > 0777 {
				return s, fmt.Errorf("invalid archive modes")
			}
		}
		seen[r.ID] = true
		switch r.Status {
		case "archiving", "archived", "restoring", "restored":
		default:
			return s, fmt.Errorf("invalid archive status")
		}
	}

	changed := false
	for i := range s.Archives {
		r := &s.Archives[i]
		if r.Status != "archiving" && r.Status != "restoring" {
			continue
		}
		archived, ae := l.archivedRevision(ArchiveDir+"/"+r.ID, Skill{Dir: r.Dir}, r.Modes)
		live, le := l.bundleRevision(Skill{Dir: r.Dir})
		switch {
		case ae == nil && archived == r.Revision && le != nil:
			r.Status = "archived"
			changed = true
		case le == nil && live == r.Revision && ae != nil:
			r.Status = "restored"
			changed = true
		default:
			return s, fmt.Errorf("archive transaction conflicts with live state")
		}
	}
	if changed {
		if err := l.savePrivate(ArchivesFile, s); err != nil {
			return s, err
		}
	}
	return s, nil
}
func (l *Library) ListArchives() ([]ArchiveReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	s, err := l.readArchives()
	return s.Archives, err
}
func (l *Library) Archive(ctx context.Context, q ArchiveRequest, p MaintenancePolicy) (ArchiveReceipt, error) {
	if err := trustedDecision(ctx); err != nil {
		return ArchiveReceipt{}, err
	}
	if p.StaleAfter == 0 {
		p.StaleAfter = 14 * 24 * time.Hour
	}
	if p.ArchiveAfter == 0 {
		p.ArchiveAfter = 30 * 24 * time.Hour
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if q.Root == "" || q.Audience != "agent" {
		return ArchiveReceipt{}, fmt.Errorf("owner agent scope required")
	}
	plans, err := l.planMaintenance(p)
	if err != nil {
		return ArchiveReceipt{}, err
	}
	s, err := l.Find(q.Name)
	if err != nil {
		return ArchiveReceipt{}, err
	}
	for _, other := range l.Index() {
		if other.Dir != s.Dir && (below(s.Dir, other.Dir) || below(other.Dir, s.Dir)) {
			return ArchiveReceipt{}, fmt.Errorf("nested skills protected")
		}
	}
	allowed := false
	for _, v := range plans {
		if v.Dir == s.Dir && v.Action == "archive_review" {
			allowed = true
		}
	}
	if !allowed {
		return ArchiveReceipt{}, fmt.Errorf("skill is protected or not due for archive review")
	}
	if err = l.writable(s); err != nil {
		return ArchiveReceipt{}, err
	}
	rev, err := l.bundleRevision(s)
	if err != nil {
		return ArchiveReceipt{}, err
	}
	if rev != q.ExpectedRevision {
		return ArchiveReceipt{}, fmt.Errorf("skill revision changed")
	}
	store, err := l.readArchives()
	if err != nil {
		return ArchiveReceipt{}, err
	}
	if len(store.Archives) >= 256 {
		return ArchiveReceipt{}, fmt.Errorf("archive store full")
	}
	sum := sha256.Sum256([]byte(s.Dir + ":" + rev + ":" + l.now().UTC().Format(time.RFC3339Nano)))
	modes, err := l.bundleModes("skills/" + s.Dir)
	if err != nil {
		return ArchiveReceipt{}, err
	}
	r := ArchiveReceipt{Modes: modes, Root: q.Root, Audience: q.Audience, ID: hex.EncodeToString(sum[:]), Dir: s.Dir, Revision: rev, Status: "archiving", CreatedAt: l.now().UTC()}
	dst, err := l.real(ArchiveDir + "/" + r.ID)
	if err != nil {
		return r, err
	}
	fd, err := fsx.OpenDirBelow(l.Home, ArchiveDir, true)
	if err != nil {
		return r, err
	}
	archiveFD := os.NewFile(uintptr(fd), ArchiveDir)
	if err = archiveFD.Chmod(0700); err != nil {
		archiveFD.Close()
		return r, err
	}
	archiveFD.Close()
	if _, err = os.Lstat(dst); !os.IsNotExist(err) {
		return r, fmt.Errorf("archive destination exists")
	}
	store.Archives = append(store.Archives, r)
	if err = l.savePrivate(ArchivesFile, store); err != nil {
		return r, err
	}
	if err = trustedDecision(ctx); err != nil {
		return r, err
	}
	if err = fsx.RenameBelow(l.Home, "skills/"+s.Dir, ArchiveDir+"/"+r.ID); err != nil {
		return r, err
	}
	r.Status = "archived"
	store.Archives[len(store.Archives)-1] = r
	return r, l.savePrivate(ArchivesFile, store)
}

// RestoreArchive moves the intact archived bundle back. Any existing destination refuses restore.
func (l *Library) RestoreArchive(ctx context.Context, id string) (ArchiveReceipt, error) {
	return l.restoreArchive(ctx, "", "", id)
}
func (l *Library) ScopedRestoreArchive(ctx context.Context, root, audience, id string) (ArchiveReceipt, error) {
	if root == "" || audience != "agent" {
		return ArchiveReceipt{}, fmt.Errorf("owner agent scope required")
	}
	return l.restoreArchive(ctx, root, audience, id)
}
func (l *Library) restoreArchive(ctx context.Context, root, audience, id string) (ArchiveReceipt, error) {
	if err := trustedDecision(ctx); err != nil {
		return ArchiveReceipt{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	store, err := l.readArchives()
	if err != nil {
		return ArchiveReceipt{}, err
	}
	for i, r := range store.Archives {
		if r.ID != id || (root != "" && (r.Root != root || r.Audience != audience)) {
			continue
		}
		if r.Status == "restored" {
			rev, e := l.bundleRevision(Skill{Dir: r.Dir})
			if e != nil || rev != r.Revision {
				return r, fmt.Errorf("restored revision no longer live")
			}
			return r, nil
		}
		if r.Status != "archived" && r.Status != "restoring" && r.Status != "archiving" {
			return r, fmt.Errorf("archive unavailable")
		}
		repo, err := l.repositoryProtected(r.Dir)
		if err != nil {
			return r, err
		}
		if l.ReadOnly(r.Dir) || maintenanceProtected(r.Dir, l.Generated) || repo {
			return r, fmt.Errorf("restore destination protected")
		}
		src := ArchiveDir + "/" + id
		archiveSkill := Skill{Dir: r.Dir}
		rev, err := l.archivedRevision(src, archiveSkill, r.Modes)
		if err != nil {
			return r, err
		}
		if rev != r.Revision {
			return r, fmt.Errorf("archive revision changed")
		}
		dst, err := l.real("skills/" + r.Dir)
		if err != nil {
			return r, err
		}
		if _, err = os.Lstat(dst); !os.IsNotExist(err) {
			return r, fmt.Errorf("restore destination exists")
		}
		for _, s := range l.Index() {
			if below(r.Dir, s.Dir) || below(s.Dir, r.Dir) {
				return r, fmt.Errorf("restore overlaps existing skill")
			}
		}
		parent := filepath.ToSlash(filepath.Dir("skills/" + r.Dir))
		fd, err := fsx.OpenDirBelow(l.Home, parent, true)
		if err != nil {
			return r, err
		}
		os.NewFile(uintptr(fd), parent).Close()
		r.Status = "restoring"
		store.Archives[i] = r
		if err = l.savePrivate(ArchivesFile, store); err != nil {
			return r, err
		}
		if err = trustedDecision(ctx); err != nil {
			return r, err
		}
		if err = l.restoreModes(src, r.Modes); err != nil {
			return r, err
		}
		if err = fsx.RenameBelow(l.Home, src, "skills/"+r.Dir); err != nil {
			return r, err
		}
		r.Status = "restored"
		store.Archives[i] = r
		return r, l.savePrivate(ArchivesFile, store)
	}
	return ArchiveReceipt{}, fmt.Errorf("unknown archive")
}

// isolatedBundle copies bounded regular bytes and original modes; no archive paths are executable input.
func (l *Library) isolatedBundle(src, dir, text string, replace bool, modes map[string]uint32) (string, error) {
	if err := os.MkdirAll(l.tmp(), 0700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(l.tmp(), "skill-revision-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	dest := "skills/" + dir
	if err = os.MkdirAll(filepath.Join(tmp, dest), 0700); err != nil {
		return "", err
	}
	entries, total := 0, 0
	err = fsx.WalkBelow(l.Home, src, func(p string, d fs.DirEntry) error {
		entries++
		if entries > 256 {
			return fmt.Errorf("bundle entry limit")
		}
		rel := strings.TrimPrefix(p, src+"/")
		if p == src {
			return nil
		}
		target := filepath.Join(tmp, dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("nonregular bundle entry")
		}
		f, e := fsx.OpenFileBelow(l.Home, p, os.O_RDONLY, 0, false)
		if e != nil {
			return e
		}
		st, e := f.Stat()
		f.Close()
		if e != nil {
			return e
		}
		b, cut, e := l.readRegular(filepath.Join(l.Home, p), MaxFile)
		if e != nil {
			return e
		}
		if cut {
			return fmt.Errorf("oversized bundle")
		}
		total += len(b)
		if total > 8<<20 {
			return fmt.Errorf("bundle byte limit")
		}
		if replace && rel == "SKILL.md" {
			b = []byte(text)
		}
		mode := st.Mode().Perm()
		if modes != nil {
			m, ok := modes[rel]
			if !ok {
				return fmt.Errorf("archive mode manifest missing file")
			}
			mode = os.FileMode(m)
		}
		if err := os.WriteFile(target, b, mode); err != nil {
			return err
		}
		return os.Chmod(target, mode)
	})
	if err != nil {
		return "", err
	}
	other := Library{Home: tmp}
	return other.bundleRevision(Skill{Dir: dir})
}
func (l *Library) revisionWithText(s Skill, text string) (string, error) {
	return l.isolatedBundle("skills/"+s.Dir, s.Dir, text, true, nil)
}
func (l *Library) archivedRevision(src string, s Skill, modes map[string]uint32) (string, error) {
	return l.isolatedBundle(src, s.Dir, "", false, modes)
}

func (l *Library) repositoryProtected(dir string) (bool, error) {
	b, cut, err := l.readRegular(filepath.Join(l.Home, BootedFile), MaxFile)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var baseline struct {
		Skills map[string]string `json:"skills"`
	}
	if cut || json.Unmarshal(b, &baseline) != nil || baseline.Skills == nil {
		return false, fmt.Errorf("invalid baseline evidence")
	}
	for p := range baseline.Skills {
		if below(dir, p) || below(p, dir) {
			return true, nil
		}
	}
	return false, nil
}

// bundleModes persists executable bits independently of nap's Raw file modes.
func (l *Library) bundleModes(src string) (map[string]uint32, error) {
	m := map[string]uint32{}
	err := fsx.WalkBelow(l.Home, src, func(p string, d fs.DirEntry) error {
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("nonregular archive entry")
		}
		if len(m) >= 256 {
			return fmt.Errorf("archive entry limit")
		}
		f, err := fsx.OpenFileBelow(l.Home, p, os.O_RDONLY, 0, false)
		if err != nil {
			return err
		}
		defer f.Close()
		var st unix.Stat_t
		if err = unix.Fstat(int(f.Fd()), &st); err != nil {
			return err
		}
		if st.Nlink != 1 {
			return fmt.Errorf("archive hardlinks refused")
		}
		rel := strings.TrimPrefix(p, src+"/")
		if clean, e := cleanRel(rel); e != nil || clean != rel {
			return fmt.Errorf("invalid archive file path")
		}
		m[rel] = uint32(st.Mode & 0777)
		return nil
	})
	return m, err
}
func (l *Library) restoreModes(src string, m map[string]uint32) error {
	for rel, mode := range m {
		f, err := fsx.OpenFileBelow(l.Home, src+"/"+rel, os.O_RDONLY, 0, false)
		if err != nil {
			return err
		}
		var st unix.Stat_t
		err = unix.Fstat(int(f.Fd()), &st)
		if err == nil && st.Nlink != 1 {
			err = fmt.Errorf("archive hardlinks refused")
		}
		if err == nil {
			err = f.Chmod(os.FileMode(mode))
		}
		if err == nil {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
