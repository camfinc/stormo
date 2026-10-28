// Package fsx has the runtime's crash-safe file write: the home is hashed by naps while the agent
// runs, so a file is either the old version or the new one, never half of each.
package fsx

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
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

// Sweep empties the home's temp dir (home/TmpDir): what a crash left there, a half-written file or
// a skill taken out of skills/ to be deleted whose files resisted removal (a directory the agent's
// commands made read-only, or their own under the uid split, which Prepare gives back to the
// runtime first). Only at boot, before anything writes through it. Everything is reached by
// descriptor and nothing is followed: a symlink is removed as itself, and a read-only directory
// is made writable through the descriptor it was opened by, never by path, so a symlink raced in
// where a directory was can never redirect the change (to the home's root, a shared directory).
func Sweep(home string) error { return SweepIn(home, TmpDir) }

// SweepIn empties the directory name in home as Sweep does (the commands' .tmp at boot: what a
// run left there is no channel to the next, docs/needletail.md §7); missing is nothing to do.
func SweepIn(home, name string) error {
	root, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: home, Err: err}
	}
	defer unix.Close(root)
	dir, err := unix.Openat(root, name, dirFlags, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: name, Err: err}
	}
	defer unix.Close(dir)
	names, err := dirNames(dir)
	if err != nil {
		return &os.PathError{Op: "readdir", Path: name, Err: err}
	}
	var first error
	for _, n := range names {
		if err := removeAt(dir, n, name+"/"+n); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// removeAt removes name in dir and, when it is a directory, everything below it, by descriptor.
func removeAt(dir int, name, p string) error {
	err := unix.Unlinkat(dir, name, 0)
	if err == nil || errors.Is(err, unix.ENOENT) {
		return nil
	}
	fd, oerr := unix.Openat(dir, name, dirFlags, 0)
	if oerr != nil {
		// not a directory (unlinkat's error stands), or one that cannot be opened
		if errors.Is(oerr, unix.ENOTDIR) || errors.Is(oerr, unix.ELOOP) {
			return &os.PathError{Op: "remove", Path: p, Err: err}
		}
		return &os.PathError{Op: "open", Path: p, Err: oerr}
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err == nil && st.Mode&0o700 != 0o700 {
		_ = unix.Fchmod(fd, 0o700) // through the descriptor: this directory, nothing else
	}
	names, err := dirNames(fd)
	if err != nil {
		return &os.PathError{Op: "readdir", Path: p, Err: err}
	}
	var first error
	for _, n := range names {
		if err := removeAt(fd, n, p+"/"+n); err != nil && first == nil {
			first = err
		}
	}
	if err := unix.Unlinkat(dir, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) && first == nil {
		first = &os.PathError{Op: "remove", Path: p, Err: err}
	}
	return first
}

// dirNames lists an open directory's entries (fd stays open).
func dirNames(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	return f.Readdirnames(-1)
}
