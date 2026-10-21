// Package fsx has the runtime's crash-safe file write: the home is hashed by naps while the agent
// runs, so a file is either the old version or the new one, never half of each.
package fsx

import (
	"os"
	"path/filepath"
)

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
