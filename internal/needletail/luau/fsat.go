package luau

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"golang.org/x/sys/unix"
)

// Every nt.fs call reaches its file descriptor by descriptor: the root (the home or a shared
// layer, a path the runtime configured) is opened, then each component of the path relative to
// the one before with O_NOFOLLOW, so a symlink anywhere below the root is refused when it is met,
// not only when the path was checked: a directory swapped for a symlink between the check and the
// open (by the terminal, or another agent in a shared layer) cannot send the call elsewhere, in or
// out of the root. (os.Root confines to the root but follows a symlink that stays inside it, even
// through O_NOFOLLOW and to create a dangling one's target, which would reach protected paths.)

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// parent is p's parent directory, open, and p's last component ("" when p is the root itself;
// then fd is the root). create makes missing directories (fsx.DirPerm) on the way. The caller closes fd.
func (p place) parent(create bool) (fd int, last string, err error) {
	fd, err = unix.Open(p.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", ioErr(p, err)
	}
	if p.rel == "" {
		return fd, "", nil
	}
	segs := strings.Split(p.rel, "/")
	for _, seg := range segs[:len(segs)-1] {
		next, err := openDir(fd, seg, create)
		if err != nil {
			unix.Close(fd)
			return -1, "", ioErr(p, err)
		}
		unix.Close(fd)
		fd = next
	}
	return fd, segs[len(segs)-1], nil
}

// openDir opens the directory name in dir, never through a symlink; create makes it when missing.
func openDir(dir int, name string, create bool) (int, error) {
	fd, err := unix.Openat(dir, name, dirFlags, 0)
	if errors.Is(err, unix.ENOENT) && create {
		if err = unix.Mkdirat(dir, name, fsx.DirPerm); err == nil || errors.Is(err, unix.EEXIST) {
			fd, err = unix.Openat(dir, name, dirFlags, 0)
		}
	}
	if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.EMLINK) {
		// O_NOFOLLOW|O_DIRECTORY on a symlink is ENOTDIR on some systems: say which it is
		var st unix.Stat_t
		if unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
			err = unix.ELOOP
		}
	}
	return fd, err
}

// openFile opens p's last component (never a symlink) with flags; create makes the directories on
// the way. Only a regular file is returned.
func (p place) openFile(flags int, create bool) (*os.File, error) {
	dir, last, err := p.parent(create)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)
	if last == "" {
		return nil, errf("not_file", "%s is not a regular file", display(p))
	}
	fd, err := unix.Openat(dir, last, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, fsx.FilePerm)
	if err != nil {
		return nil, ioErr(p, err)
	}
	f := os.NewFile(uintptr(fd), display(p))
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errf("not_file", "%s is not a regular file", display(p))
	}
	return f, nil
}

// lstat describes p itself (a symlink as one); a missing p (or parent) is fs.ErrNotExist.
func (p place) lstat() (*unix.Stat_t, error) {
	dir, last, err := p.parent(false)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)
	var st unix.Stat_t
	if last == "" {
		err = unix.Fstat(dir, &st)
	} else {
		err = unix.Fstatat(dir, last, &st, unix.AT_SYMLINK_NOFOLLOW)
	}
	if err != nil {
		return nil, ioErr(p, err)
	}
	return &st, nil
}

// remove unlinks p, a regular file, relative to its parent.
func (p place) remove() error {
	dir, last, err := p.parent(false)
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	var st unix.Stat_t
	if last == "" {
		return errf("not_file", "%s is not a regular file", display(p))
	}
	if err := unix.Fstatat(dir, last, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return ioErr(p, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return errf("not_file", "%s is not a regular file", display(p))
	}
	if err := unix.Unlinkat(dir, last, 0); err != nil {
		return ioErr(p, err)
	}
	return nil
}

// listBatch is how many entries fs.list reads at a time.
const listBatch = 128

// list is p's entries, at most limit, read in batches and stopped when ctx ends.
func (p place) list(ctx context.Context, limit int) ([]map[string]any, error) {
	dir, last, err := p.parent(false)
	if err != nil {
		return nil, err
	}
	fd := dir
	if last != "" {
		fd, err = openDir(dir, last, false)
		unix.Close(dir)
		if errors.Is(err, unix.ENOTDIR) {
			return nil, errf("not_dir", "%s is not a directory", display(p))
		}
		if err != nil {
			return nil, ioErr(p, err)
		}
	}
	f := os.NewFile(uintptr(fd), display(p))
	defer f.Close()
	out := []map[string]any{}
	for len(out) < limit {
		if ctx.Err() != nil {
			return nil, errf("cancelled", "fs.list %s: the run ended", display(p))
		}
		ents, err := f.ReadDir(min(listBatch, limit-len(out)))
		for _, e := range ents {
			var st unix.Stat_t
			if unix.Fstatat(fd, e.Name(), &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
				continue
			}
			out = append(out, map[string]any{"name": e.Name(), "type": statKind(&st), "size": st.Size})
		}
		if errors.Is(err, io.EOF) || len(ents) == 0 {
			break
		}
		if err != nil {
			return nil, ioErr(p, err)
		}
	}
	return out, nil
}

func statKind(st *unix.Stat_t) string {
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFREG:
		return "file"
	case unix.S_IFDIR:
		return "dir"
	case unix.S_IFLNK:
		return "symlink"
	}
	return "other"
}
