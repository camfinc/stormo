package core

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Shared space across machines (docs/remote-core.md §5). The core's workdir is the source of
// truth. A runner keeps its host's copy of the layers its agents use in step with it through this
// API: a listing with content hashes, reads, and writes and deletes that only land when the
// file is still what the runner last saw (else 409, and the runner keeps its version as a
// conflict copy). Writes respect other agents' hard and temp locks. They are not attributed
// here; the core's scan attributes them like any change on disk, with the agents' fs_note claims.

// SyncEntry is one file of a layer as the sync sees it.
type SyncEntry struct {
	Path   string `json:"path"` // relative to the layer, slash-separated
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

const maxSyncFile = 64 << 20

var syncRoute = regexp.MustCompile(`^/api/sync/([a-z][a-z0-9-]*)(/.*)?$`)

// skipSync are files the sync never carries: the core's and the sync's own temporaries.
func skipSync(name string) bool {
	return strings.HasPrefix(name, ".fs_write-") || strings.HasPrefix(name, ".stormo-sync-")
}

func syncHash(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// SyncList is a layer's files (links are not followed; files over the cap are left out).
func (w *Workdir) SyncList(layer string) ([]SyncEntry, error) {
	dir := filepath.Join(w.o.Root, layer)
	out := []SyncEntry{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || skipSync(d.Name()) {
			return nil
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxSyncFile {
			return nil
		}
		sum, size, err := syncHash(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out = append(out, SyncEntry{Path: filepath.ToSlash(rel), SHA256: sum, Size: size})
		return nil
	})
	return out, err
}

// syncKey checks a layer-relative path and gives its workdir key.
func syncKey(layer, rel string) (string, error) {
	rel = strings.TrimPrefix(rel, "/")
	clean := path.Clean(rel)
	if rel == "" || clean != rel || strings.HasPrefix(clean, "../") || clean == ".." || strings.ContainsRune(rel, 0) || skipSync(path.Base(clean)) {
		return "", toolErr("bad path %q", rel)
	}
	return layer + "/" + clean, nil
}

// errStale: the file is not what the caller last saw.
var errStale = errors.New("stale")

func (w *Workdir) currentSHA(root *os.Root, rel string) (string, error) {
	st, err := root.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", toolErr("not a regular file")
	}
	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// lockedAgainst is a hard or temp lock on key held by an agent outside allowed (the placement's
// own agents), nil when none.
func (w *Workdir) lockedAgainst(key string, allowed map[string]bool) (*lockRow, error) {
	rows, err := w.covering(key, "")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if (r.kind == "hard" || r.kind == "temp") && !allowed[r.owner] {
			return &r, nil
		}
	}
	return nil, nil
}

// SyncPut writes a layer file when it is still base (the runner's last-seen hash; "" for a new
// file). sha256 of the new content comes back.
func (w *Workdir) SyncPut(layer, rel string, data []byte, base string, allowed map[string]bool) (string, error) {
	key, err := syncKey(layer, rel)
	if err != nil {
		return "", err
	}
	w.writeMus.Lock()
	defer w.writeMus.Unlock()
	if r, err := w.lockedAgainst(key, allowed); err != nil {
		return "", err
	} else if r != nil {
		return "", toolErr("locked: %s", describe(*r))
	}
	root, rel, err := w.open(key)
	if err != nil {
		return "", err
	}
	defer root.Close()
	cur, err := w.currentSHA(root, rel)
	if err != nil {
		return "", confined(root, key, rel, err)
	}
	if cur != base {
		return "", errStale
	}
	if err := root.MkdirAll(filepath.Dir(rel), 0o775); err != nil {
		return "", confined(root, key, rel, err)
	}
	tmpName := filepath.Join(filepath.Dir(rel), fmt.Sprintf(".stormo-sync-%d", time.Now().UnixNano()))
	tmp, err := root.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o664)
	if err != nil {
		return "", confined(root, key, rel, err)
	}
	defer root.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := root.Rename(tmpName, rel); err != nil {
		return "", confined(root, key, rel, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// SyncDelete removes a layer file when it is still base.
func (w *Workdir) SyncDelete(layer, rel, base string, allowed map[string]bool) error {
	key, err := syncKey(layer, rel)
	if err != nil {
		return err
	}
	w.writeMus.Lock()
	defer w.writeMus.Unlock()
	if r, err := w.lockedAgainst(key, allowed); err != nil {
		return err
	} else if r != nil {
		return toolErr("locked: %s", describe(*r))
	}
	root, rel, err := w.open(key)
	if err != nil {
		return err
	}
	defer root.Close()
	cur, err := w.currentSHA(root, rel)
	if err != nil {
		return confined(root, key, rel, err)
	}
	if cur == "" {
		return nil
	}
	if cur != base {
		return errStale
	}
	return confined(root, key, rel, root.Remove(rel))
}

// SyncRead reads a layer file.
func (w *Workdir) SyncRead(layer, rel string) ([]byte, error) {
	key, err := syncKey(layer, rel)
	if err != nil {
		return nil, err
	}
	root, rel, err := w.open(key)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	st, err := root.Lstat(rel)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > maxSyncFile {
		return nil, toolErr("not a regular file under %d MB", maxSyncFile>>20)
	}
	b, err := root.ReadFile(rel)
	return b, confined(root, key, rel, err)
}

// handleSync serves /api/sync/<layer>[/<path>] to a runner (operate token, X-Stormo-Placement):
// only the layers of the agents its placement runs, plus group.
func (c *Core) handleSync(w http.ResponseWriter, r *http.Request, who Caller, roster func() []BusAgent) {
	m := syncRoute.FindStringSubmatch(r.URL.Path)
	if m == nil {
		writeJSONBody(w, 404, errBody("not_found", r.URL.Path))
		return
	}
	if !who.Can(ScopeOperate) {
		writeJSONBody(w, 403, errBody("forbidden", "sync takes a runner's operate token"))
		return
	}
	layer, rel := m[1], strings.TrimPrefix(m[2], "/")
	placement := r.Header.Get("X-Stormo-Placement")
	p, ok := c.inst.PlacementNamed(placement)
	if !ok || p.Kind == "local" {
		writeJSONBody(w, 400, errBody("bad_request", "X-Stormo-Placement names the runner placement syncing"))
		return
	}
	allowed, layers := map[string]bool{}, map[string]bool{"group": true}
	for _, a := range roster() {
		if i := c.Links.Info(a.ID); i != nil && i.Placement == p.Name {
			allowed[a.ID], layers[a.Unit] = true, true
		}
	}
	if !layers[layer] {
		writeJSONBody(w, 403, errBody("forbidden", "no agent of "+p.Name+" uses the "+layer+" layer"))
		return
	}
	fail := func(err error) {
		var te ToolError
		switch {
		case errors.Is(err, errStale):
			writeJSONBody(w, 409, errBody("stale", "the file changed since you last saw it"))
		case errors.Is(err, fs.ErrNotExist):
			writeJSONBody(w, 404, errBody("not_found", "no such file"))
		case errors.As(err, &te):
			writeJSONBody(w, 423, errBody("refused", te.Error()))
		default:
			writeJSONBody(w, 500, errBody("core_internal", clip(err.Error(), 300)))
		}
	}
	switch {
	case rel == "" && r.Method == http.MethodGet:
		files, err := c.Workdir.SyncList(layer)
		if err != nil {
			fail(err)
			return
		}
		writeJSONBody(w, 200, map[string]any{"layer": layer, "files": files})
	case r.Method == http.MethodGet:
		b, err := c.Workdir.SyncRead(layer, rel)
		if err != nil {
			fail(err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
	case r.Method == http.MethodPut:
		b, err := io.ReadAll(io.LimitReader(r.Body, maxSyncFile+1))
		if err != nil || len(b) > maxSyncFile {
			writeJSONBody(w, 413, errBody("too_large", fmt.Sprintf("at most %d MB", maxSyncFile>>20)))
			return
		}
		sum, err := c.Workdir.SyncPut(layer, rel, b, r.Header.Get("X-Base-SHA256"), allowed)
		if err != nil {
			fail(err)
			return
		}
		writeJSONBody(w, 200, map[string]any{"sha256": sum})
	case r.Method == http.MethodDelete:
		if err := c.Workdir.SyncDelete(layer, rel, r.Header.Get("X-Base-SHA256"), allowed); err != nil {
			fail(err)
			return
		}
		writeJSONBody(w, 200, map[string]any{"deleted": rel})
	default:
		writeJSONBody(w, 405, errBody("method_not_allowed", r.Method))
	}
}
