package link

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Syncer keeps a host's copy of shared-space layers in step with the core's (docs/remote-core.md
// §5): a three-way comparison per file between this copy, the core's and what the two last agreed
// on. One side changed: it wins. Both changed: the core's version is kept and this host's goes
// back as a conflict copy next to it, never merged silently.
type Syncer struct {
	CoreURL   string
	Token     string
	Placement string
	// Dir holds the layers (<Dir>/<layer>/…), as local compose mounts them.
	Dir string
	// Layers to try; the core answers 403 for ones this placement may not sync.
	Layers func() []string
	// StateFile keeps what each side last agreed on (outside Dir).
	StateFile string
	Client    *http.Client
	Logf      func(format string, args ...any)
	Now       func() time.Time
}

type remoteFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// state: layer → path → sha both sides agreed on.
type syncState map[string]map[string]string

func (s *Syncer) load() syncState {
	st := syncState{}
	if b, err := os.ReadFile(s.StateFile); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (s *Syncer) save(st syncState) {
	b, _ := json.Marshal(st)
	_ = os.MkdirAll(filepath.Dir(s.StateFile), 0o700)
	tmp := s.StateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, s.StateFile)
	}
}

// Run syncs every interval until ctx ends.
func (s *Syncer) Run(ctx context.Context, every time.Duration) {
	for {
		if err := s.Once(ctx); err != nil && ctx.Err() == nil {
			s.Logf("sync: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Once is one round over every layer.
func (s *Syncer) Once(ctx context.Context) error {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Logf == nil {
		s.Logf = func(string, ...any) {}
	}
	st := s.load()
	defer s.save(st)
	var errs []error
	for _, layer := range s.Layers() {
		if st[layer] == nil {
			st[layer] = map[string]string{}
		}
		if err := s.layer(ctx, layer, st[layer]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", layer, err))
		}
	}
	return errors.Join(errs...)
}

// errRefused: the core will not take this write as it stands (stale, or locked); a retry will not help.
var errRefused = errors.New("refused")

func (s *Syncer) do(ctx context.Context, method, layer, rel string, body []byte, base string) (*http.Response, error) {
	u := strings.TrimRight(s.CoreURL, "/") + "/api/sync/" + layer
	if rel != "" {
		u += "/" + (&url.URL{Path: rel}).EscapedPath()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("X-Stormo-Placement", s.Placement)
	if method == http.MethodPut || method == http.MethodDelete {
		req.Header.Set("X-Base-SHA256", base)
	}
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 2 * time.Minute}
	}
	return c.Do(req)
}

func (s *Syncer) layer(ctx context.Context, layer string, agreed map[string]string) error {
	res, err := s.do(ctx, http.MethodGet, layer, "", nil, "")
	if err != nil {
		return err
	}
	if res.StatusCode == http.StatusForbidden {
		res.Body.Close()
		return nil // not one of this placement's layers
	}
	var list struct {
		Files []remoteFile `json:"files"`
	}
	err = json.NewDecoder(res.Body).Decode(&list)
	res.Body.Close()
	if res.StatusCode != 200 || err != nil {
		return fmt.Errorf("listing: HTTP %d %v", res.StatusCode, err)
	}
	remote := map[string]string{}
	for _, f := range list.Files {
		remote[f.Path] = f.SHA256
	}
	dir := filepath.Join(s.Dir, layer)
	local, err := localHashes(dir)
	if err != nil {
		return err
	}
	paths := map[string]bool{}
	for p := range remote {
		paths[p] = true
	}
	for p := range local {
		paths[p] = true
	}
	for p := range agreed {
		paths[p] = true
	}
	for p := range paths {
		l, r, a := local[p], remote[p], agreed[p]
		switch {
		case l == r:
			if l == "" {
				delete(agreed, p)
			} else {
				agreed[p] = l
			}
		case l == a: // only the core changed it
			if r == "" {
				_ = os.Remove(filepath.Join(dir, filepath.FromSlash(p)))
				delete(agreed, p)
			} else if err := s.pull(ctx, layer, dir, p); err == nil {
				agreed[p] = r
			}
		case r == a: // only this host changed it
			if l == "" {
				if s.push(ctx, http.MethodDelete, layer, dir, p, a) == nil {
					delete(agreed, p)
				}
			} else if err := s.push(ctx, http.MethodPut, layer, dir, p, a); err == nil {
				agreed[p] = l
			} else if errors.Is(err, errRefused) {
				// The core moved on (409) or someone holds a lock on it (423): keep both.
				s.conflict(ctx, layer, dir, p, r, agreed)
			}
		default: // both changed it
			s.conflict(ctx, layer, dir, p, r, agreed)
		}
	}
	return nil
}

// conflict keeps this host's version as a conflict copy (sent to the core too) and takes the
// core's.
func (s *Syncer) conflict(ctx context.Context, layer, dir, p, remoteSHA string, agreed map[string]string) {
	file := filepath.Join(dir, filepath.FromSlash(p))
	if b, err := os.ReadFile(file); err == nil {
		ext := filepath.Ext(p)
		cp := strings.TrimSuffix(p, ext) + fmt.Sprintf(".conflict-%s-%s", s.Placement, s.Now().UTC().Format("20060102T150405")) + ext
		cfile := filepath.Join(dir, filepath.FromSlash(cp))
		if os.WriteFile(cfile, b, 0o664) == nil && s.push(ctx, http.MethodPut, layer, dir, cp, "") == nil {
			sum := sha256.Sum256(b)
			agreed[cp] = hex.EncodeToString(sum[:])
		}
		s.Logf("sync: %s/%s changed on both sides; this host's version is %s", layer, p, cp)
	}
	if remoteSHA == "" {
		_ = os.Remove(file)
		delete(agreed, p)
		return
	}
	if s.pull(ctx, layer, dir, p) == nil {
		agreed[p] = remoteSHA
	}
}

func (s *Syncer) pull(ctx context.Context, layer, dir, p string) error {
	res, err := s.do(ctx, http.MethodGet, layer, p, nil, "")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("pull %s: HTTP %d", p, res.StatusCode)
	}
	file := filepath.Join(dir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(file), 0o775); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(file), fmt.Sprintf(".stormo-sync-%d", time.Now().UnixNano()))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o664)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, res.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, file)
}

func (s *Syncer) push(ctx context.Context, method, layer, dir, p, base string) error {
	var body []byte
	if method == http.MethodPut {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return err
		}
		body = b
	}
	res, err := s.do(ctx, method, layer, p, body, base)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		err := fmt.Errorf("%s %s: HTTP %d %s", method, p, res.StatusCode, strings.TrimSpace(string(b)))
		if res.StatusCode == http.StatusConflict || res.StatusCode == http.StatusLocked {
			return fmt.Errorf("%w: %v", errRefused, err)
		}
		return err
	}
	return nil
}

// localHashes hashes a layer's regular files (links not followed; sync temporaries skipped).
func localHashes(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		name := d.Name()
		if strings.HasPrefix(name, ".stormo-sync-") || strings.HasPrefix(name, ".fs_write-") {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return out, err
}
