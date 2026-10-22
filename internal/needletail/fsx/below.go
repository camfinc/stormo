package fsx

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// Writes below a root the agent's own commands can also write (the home's skills/ and
// scripts/, a shared layer) reach their directory by descriptor: the root is opened (a path the
// runtime configured), then each directory below it relative to the one before with O_NOFOLLOW.
// A directory swapped for a symlink between the runtime's check of a path and its write (a
// command racing it in the background) makes the write fail; it never lands where the link
// points, such as the runtime's own cron/ or state/ (docs/needletail.md §7).

// ErrLink is a symlink met below the root.
var ErrLink = errors.New("a symlink is in the way (the runtime never writes through one)")

// ErrNotRegular is a file that is not a regular one where one is opened (a FIFO, a device, a
// directory); ErrTooBig a file over what ReadFileBelow reads.
var (
	ErrNotRegular = errors.New("not a regular file")
	ErrTooBig     = errors.New("over the size limit")
)

// fileError is rel's error, matching sentinel (errors.Is), with msg as its text.
type fileError struct {
	msg      string
	sentinel error
}

func (e *fileError) Error() string        { return e.msg }
func (e *fileError) Is(target error) bool { return target == e.sentinel }

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// DirPerm and FilePerm are what the runtime gives what it makes in the agent's workspace
// (skills/, scripts/, the files it writes for the agent): writable by the group, which the
// agent's commands share, so the terminal can change them too.
const (
	DirPerm  = 0o775
	FilePerm = 0o664
)

func split(rel string) ([]string, error) {
	rel = filepath.ToSlash(rel)
	if rel == "" || rel == "." {
		return nil, nil
	}
	if path.IsAbs(rel) || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("%s: not a clean path below the root", rel)
	}
	return strings.Split(rel, "/"), nil
}

// OpenDirBelow opens the directory rel (slash-separated, "" for root itself) below root, never
// through a symlink below root; create makes the missing ones (DirPerm). The caller closes fd.
func OpenDirBelow(root, rel string, create bool) (int, error) {
	segs, err := split(rel)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: root, Err: err}
	}
	for i, seg := range segs {
		next, err := unix.Openat(fd, seg, dirFlags, 0)
		if errors.Is(err, unix.ENOENT) && create {
			if err = unix.Mkdirat(fd, seg, DirPerm); err == nil || errors.Is(err, unix.EEXIST) {
				next, err = unix.Openat(fd, seg, dirFlags, 0)
			}
		}
		if err != nil {
			link := isLink(fd, seg) || errors.Is(err, unix.ELOOP)
			unix.Close(fd)
			p := path.Join(segs[:i+1]...)
			if link {
				return -1, fmt.Errorf("%s: %w", p, ErrLink)
			}
			return -1, &os.PathError{Op: "open", Path: p, Err: err}
		}
		unix.Close(fd)
		fd = next
	}
	return fd, nil
}

func isLink(dir int, name string) bool {
	var st unix.Stat_t
	return unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK
}

// parent opens rel's parent below root and returns rel's last component.
func parent(root, rel string, create bool) (int, string, error) {
	segs, err := split(rel)
	if err != nil {
		return -1, "", err
	}
	if len(segs) == 0 {
		return -1, "", fmt.Errorf("%s: no file below the root", root)
	}
	fd, err := OpenDirBelow(root, path.Join(segs[:len(segs)-1]...), create)
	return fd, segs[len(segs)-1], err
}

// WriteAtomicBelow writes rel below root as WriteAtomicIn does (a fsynced temporary file in
// tmpDir, then a rename), the rename going into rel's directory reached by descriptor; missing
// directories are made (DirPerm). The final component is replaced, never followed.
func WriteAtomicBelow(root, rel, tmpDir string, body []byte, perm os.FileMode) error {
	dir, last, err := parent(root, rel, true)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(tmpDir, "."+last+".tmp-*")
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
		err = unix.Renameat(unix.AT_FDCWD, name, dir, last)
	}
	if err != nil {
		_ = os.Remove(name)
		return &os.PathError{Op: "write", Path: rel, Err: err}
	}
	_ = unix.Fsync(dir)
	return nil
}

// OpenFileBelow opens rel below root with flags (never O_TRUNC: truncate once the file is known
// to be one), never through a symlink, the last component included; create makes the missing
// directories. Only a regular file is returned.
func OpenFileBelow(root, rel string, flags int, perm os.FileMode, create bool) (*os.File, error) {
	dir, last, err := parent(root, rel, create)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)
	fd, err := unix.Openat(dir, last, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		if isLink(dir, last) || errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%s: %w", rel, ErrLink)
		}
		return nil, &os.PathError{Op: "open", Path: rel, Err: err}
	}
	f := os.NewFile(uintptr(fd), filepath.Join(root, rel))
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, &fileError{rel + " is not a regular file", ErrNotRegular}
	}
	return f, nil
}

// RemoveBelow removes rel below root: a regular file or a symlink (never what it points to), or,
// with dir, an empty directory.
func RemoveBelow(root, rel string, dir bool) error {
	fd, last, err := parent(root, rel, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	flags := 0
	if dir {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(fd, last, flags); err != nil {
		return &os.PathError{Op: "remove", Path: rel, Err: err}
	}
	return nil
}

// RenameOut moves rel below root (a file or a directory, never followed) to dst, an absolute path
// in a directory only the runtime writes (the temp dir).
func RenameOut(root, rel, dst string) error {
	fd, last, err := parent(root, rel, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Renameat(fd, last, unix.AT_FDCWD, dst); err != nil {
		return &os.PathError{Op: "rename", Path: rel, Err: err}
	}
	return nil
}

// ReadFileBelow reads rel below root, a regular file of at most max bytes, by descriptor: every
// directory below root and the file itself opened with O_NOFOLLOW, the file non-blocking and
// checked a regular file on what was opened (OpenFileBelow), and at most max+1 bytes read. A
// FIFO or a device the agent's commands swap in after a check (a symlink to /dev/zero) can then
// neither block the read nor feed it without end.
func ReadFileBelow(root, rel string, max int64) ([]byte, error) {
	f, err := OpenFileBelow(root, rel, unix.O_RDONLY, 0, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, &os.PathError{Op: "read", Path: rel, Err: err}
	}
	if int64(len(b)) > max {
		return nil, &fileError{fmt.Sprintf("%s is over %d bytes", rel, max), ErrTooBig}
	}
	return b, nil
}

// ReadDirBelow lists the directory rel below root (reached as OpenDirBelow does), sorted by name.
func ReadDirBelow(root, rel string) ([]os.DirEntry, error) {
	fd, err := OpenDirBelow(root, rel, false)
	if err != nil {
		return nil, err
	}
	d := os.NewFile(uintptr(fd), filepath.Join(root, rel))
	defer d.Close()
	ents, err := d.ReadDir(-1)
	slices.SortFunc(ents, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return ents, err
}

// WalkBelow walks the directory rel below root by descriptor, depth first in name order: fn is
// called for every entry below it (not rel itself), with its path relative to root
// (slash-separated). A directory is entered after fn returns nil for it (fs.SkipDir leaves it
// out; fs.SkipAll ends the walk); a directory that cannot be opened is left out. Nothing is
// followed: rel is reached as OpenDirBelow does, a symlink is passed to fn as itself, and each
// directory is opened relative to its parent's descriptor with O_NOFOLLOW, so one swapped for a
// symlink after it was listed is not entered (filepath.WalkDir and os.Root would read the
// directory it points to). Only the error of opening rel is returned (or fn's).
func WalkBelow(root, rel string, fn func(rel string, d fs.DirEntry) error) error {
	fd, err := OpenDirBelow(root, rel, false)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if rel == "." {
		rel = ""
	}
	if err := walkFD(fd, filepath.Join(root, filepath.FromSlash(rel)), rel, fn); err != nil && err != fs.SkipAll {
		return err
	}
	return nil
}

func walkFD(fd int, abs, rel string, fn func(string, fs.DirEntry) error) error {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil
	}
	d := os.NewFile(uintptr(dup), abs)
	ents, _ := d.ReadDir(-1)
	d.Close()
	slices.SortFunc(ents, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, e := range ents {
		p := path.Join(rel, e.Name())
		err := fn(p, e)
		switch {
		case err == fs.SkipDir:
			continue
		case err != nil:
			return err
		case !e.IsDir():
			continue
		}
		child, err := unix.Openat(fd, e.Name(), dirFlags, 0)
		if err != nil {
			continue // gone, or no longer a directory (a symlink now): not entered
		}
		err = walkFD(child, filepath.Join(abs, e.Name()), p, fn)
		unix.Close(child)
		if err != nil {
			return err
		}
	}
	return nil
}
