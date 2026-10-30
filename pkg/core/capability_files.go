package core

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/camfinc/stormo/pkg/shared"
)

// Capability jobs take files from the agent's shared space and give files back (docs/core.md,
// "Capabilities"). The core copies them through its own disk, only within the layers the agent
// reaches (group and its unit), through the same confined roots as every other file operation.

const (
	maxJobFiles  = 20
	maxStageSize = 4 << 30
	maxStageN    = 20000
)

// skipDirs are not copied into a job: dependencies the worker brings itself, and history.
var skipDirs = map[string]bool{"node_modules": true, ".git": true}

// JobInputs checks a job's input paths for agent: in its layers, existing, distinct base names.
// It gives them as agents see them (/shared/<layer>/…).
func (w *Workdir) JobInputs(agent string, paths []string) ([]string, error) {
	if len(paths) > maxJobFiles {
		return nil, toolErr("at most %d files per job", maxJobFiles)
	}
	out := []string{}
	bases := map[string]bool{}
	for _, p := range paths {
		key, _, err := w.resolve(agent, p, false)
		if err != nil {
			return nil, err
		}
		if hasMeta(key) {
			return nil, toolErr("%s: name files, not globs", p)
		}
		root, rel, err := w.open(key)
		if err != nil {
			return nil, err
		}
		_, err = root.Stat(rel)
		root.Close()
		if err != nil {
			return nil, toolErr("%s: no such file or directory in the shared space", shown(key))
		}
		base := path.Base(key)
		if bases[base] {
			return nil, toolErr("two inputs are named %s: a job takes each base name once", base)
		}
		bases[base] = true
		out = append(out, shown(key))
	}
	return out, nil
}

// stageLocal copies a local file or folder to dst with the rules StageIn follows (the owner's
// `stormo capability try`).
func stageLocal(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		root, err := os.OpenRoot(filepath.Dir(src))
		if err != nil {
			return err
		}
		defer root.Close()
		return copyFrom(root, filepath.Base(src), dst)
	}
	root, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink != 0 {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		if d.IsDir() {
			if p != "." && skipDirs[d.Name()] {
				return fs.SkipDir
			}
			return os.MkdirAll(target, 0o775)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFrom(root, filepath.FromSlash(p), target)
	})
}

// JobOut checks the folder a job's files go to: in agent's layers, not a file.
func (w *Workdir) JobOut(agent, p string) (string, error) {
	key, _, err := w.resolve(agent, p, false)
	if err != nil {
		return "", err
	}
	if hasMeta(key) {
		return "", toolErr("out is a folder, not a glob")
	}
	root, rel, err := w.open(key)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if st, err := root.Lstat(rel); err == nil && !st.IsDir() {
		return "", toolErr("%s is a file; out is a folder", shown(key))
	}
	return shown(key), nil
}

// StageIn copies a job's inputs (checked by JobInputs) into dir, each under its base name;
// folders are copied whole except node_modules and .git, links are left out.
func (w *Workdir) StageIn(agent string, paths []string, dir string) error {
	var size int64
	n := 0
	for _, p := range paths {
		key, _, err := w.resolve(agent, p, false)
		if err != nil {
			return err
		}
		root, rel, err := w.open(key)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, path.Base(key))
		err = fs.WalkDir(root.FS(), filepath.ToSlash(rel), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			sub, _ := filepath.Rel(rel, filepath.FromSlash(p))
			target := filepath.Join(dst, sub)
			if d.IsDir() {
				if p != filepath.ToSlash(rel) && skipDirs[d.Name()] {
					return fs.SkipDir
				}
				return os.MkdirAll(target, 0o775)
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if n++; n > maxStageN {
				return toolErr("over %d files to hand over", maxStageN)
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if size += info.Size(); size > maxStageSize {
				return toolErr("over %d GB to hand over", maxStageSize>>30)
			}
			return copyFrom(root, filepath.FromSlash(p), target)
		})
		root.Close()
		if err != nil {
			return fmt.Errorf("handing over %s: %w", shown(key), err)
		}
	}
	return nil
}

func copyFrom(root *os.Root, rel, target string) error {
	src, err := root.Open(rel)
	if err != nil {
		return err
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o775); err != nil {
		return err
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o664)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

// Deliver copies the files a job wrote under src into the agent's folder out (a shared path checked
// by JobOut), keeping their layout, and records each as written by agent through how. A file under
// someone else's hard or temporary lock is not written. It gives the delivered paths.
func (w *Workdir) Deliver(agent, out, src, how string) ([]string, error) {
	outKey, _, err := w.resolve(agent, out, false)
	if err != nil {
		return nil, err
	}
	root, outRel, err := w.open(outKey)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	delivered := []string{}
	var size int64
	err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return err
		}
		if len(delivered) >= maxStageN {
			return toolErr("over %d files written", maxStageN)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if size += info.Size(); size > 2*maxStageSize {
			return toolErr("over %d GB written", 2*maxStageSize>>30)
		}
		sub, _ := filepath.Rel(src, p)
		rel := filepath.Join(outRel, sub)
		key := outKey + "/" + filepath.ToSlash(sub)
		// One file at a time under the write lock: a large delivery does not hold up fs_write.
		w.writeMus.Lock()
		defer w.writeMus.Unlock()
		locks, err := w.covering(key, agent)
		if err != nil {
			return err
		}
		for _, r := range locks {
			if r.kind == "hard" || r.kind == "temp" {
				return toolErr("%s not written: %s", shown(key), describe(r))
			}
		}
		st, statErr := root.Lstat(rel)
		exists := statErr == nil
		if exists && (st.Mode()&fs.ModeSymlink != 0 || st.IsDir()) {
			return toolErr("%s is a link or a folder; not overwritten", shown(key))
		}
		if err := root.MkdirAll(filepath.Dir(rel), 0o775); err != nil {
			return confined(root, key, rel, err)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		tmp := filepath.Join(filepath.Dir(rel), ".cap-"+filepath.Base(rel))
		f, err := root.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o664)
		if err != nil {
			return confined(root, key, rel, err)
		}
		if _, err := io.Copy(f, in); err != nil {
			f.Close()
			root.Remove(tmp)
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		if err := root.Rename(tmp, rel); err != nil {
			return confined(root, key, rel, err)
		}
		kind := "created"
		if exists {
			kind = "modified"
		}
		if err := w.record(key, w.host(key), kind, agent, how, "", w.o.Now().UnixMilli()); err != nil {
			return err
		}
		delivered = append(delivered, shown(key))
		return nil
	})
	return delivered, err
}

// unitOf is agent's unit layer (group when it has none).
func (w *Workdir) unitOf(agent string) string {
	if a, ok := w.agent(agent); ok && a.Unit != "" {
		return a.Unit
	}
	return "group"
}

// jobOutBase holds agent's job folders of a capability.
func (w *Workdir) jobOutBase(agent, capability string) string {
	return fmt.Sprintf("%s/%s/capabilities/%s", shared.Root, w.unitOf(agent), strings.ReplaceAll(capability, ".", "-"))
}

// jobOutDefault is where a job's files go when the agent names no folder.
func (w *Workdir) jobOutDefault(agent, capability string, job int64) string {
	return fmt.Sprintf("%s/job-%d", w.jobOutBase(agent, capability), job)
}
