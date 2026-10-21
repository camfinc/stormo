// Package fsx has the runtime's crash-safe file write: the home is hashed by naps while the agent
// runs, so a file is either the old version or the new one, never half of each.
package fsx

import (
	"io/fs"
	"os"
	"path/filepath"
)

// TmpDir is the home's directory for temporary files, relative to the home: no snapshot rule
// claims it, so a half-written file (a nap taken mid-write, a crash before the rename) never
// reaches a nap. Files under a claimed glob (skills/, sessions/, state/needletail/) are written
// through it with WriteAtomicIn.
const TmpDir = ".needletail-tmp"

// WriteAtomic writes path through a fsynced temporary file in the same directory and a rename,
// then fsyncs the directory. perm is the new file's mode.
func WriteAtomic(path string, body []byte, perm os.FileMode) error {
	return WriteAtomicIn(path, filepath.Dir(path), body, perm)
}

// WriteAtomicIn is WriteAtomic with the temporary file in tmpDir (made 0700 if missing), which
// must be on the same filesystem. Use it where a snapshot rule claims every file beside path
// (skills/[!.]*/**): a nap taken mid-write, or a crash before the rename, would otherwise keep
// the temporary file as part of the skill.
func WriteAtomicIn(path, tmpDir string, body []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(body)
	if err == nil {
		err = tmp.Chmod(perm)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// RemoveAll removes path and everything below it, also what directories made read-only hold
// (they are made writable first). It is for the home's own trees, never followed through links.
func RemoveAll(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700)
		}
		return nil
	})
	return os.RemoveAll(path)
}

// Sweep empties the home's temp dir (home/TmpDir): what a crash left there, a half-written file or
// a skill taken out of skills/ to be deleted. Only at boot, before anything writes through it.
func Sweep(home string) error {
	dir := filepath.Join(home, TmpDir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var first error
	for _, e := range entries {
		if err := RemoveAll(filepath.Join(dir, e.Name())); err != nil && first == nil {
			first = err
		}
	}
	return first
}
