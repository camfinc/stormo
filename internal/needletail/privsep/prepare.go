package privsep

import (
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Prepare lays the home out for the split (as root, before Drop): the agent's workspace (every
// entry not the runtime's) to the exec user, group-writable; the home itself to the runtime,
// RootMode; the runtime's directories made when missing and, with all they hold, to the runtime
// (Private 0700/0600, ReadOnly 0750/0640); its root files (Files) to the runtime with their modes.
// The runtime's entries come last, so an inode linked from the workspace ends up the runtime's.
// Everything is reached by descriptor and never through a symlink (a symlink is given to its
// owner as itself). A runtime name that is not a directory (a file, a symlink) is moved aside
// as <name>.moved-<time> and logged; a root file that is not a regular file fails the boot.
func Prepare(home string, u Users, logf func(string)) error {
	root, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return &os.PathError{Op: "open", Path: home, Err: err}
	}
	defer unix.Close(root)
	names, err := readNames(root)
	if err != nil {
		return err
	}
	runtimeTop := map[string]bool{}
	for _, d := range append(append([]string{}, Private...), ReadOnly...) {
		top, _, _ := strings.Cut(d, "/")
		runtimeTop[top] = true
	}
	for f := range Files {
		runtimeTop[f] = true
	}
	for _, n := range names {
		if runtimeTop[n] {
			continue
		}
		if err := fixTree(root, n, n, u.ExecUID, u.RuntimeGID, workspaceMode); err != nil {
			return err
		}
	}
	if err := unix.Fchown(root, u.RuntimeUID, u.RuntimeGID); err != nil {
		return &os.PathError{Op: "chown", Path: home, Err: err}
	}
	if err := unix.Fchmod(root, RootMode); err != nil {
		return &os.PathError{Op: "chmod", Path: home, Err: err}
	}
	for _, d := range Private {
		if err := runtimeDir(root, d, u, privateMode, logf); err != nil {
			return err
		}
	}
	for _, d := range ReadOnly {
		if err := runtimeDir(root, d, u, readOnlyMode, logf); err != nil {
			return err
		}
	}
	files := make([]string, 0, len(Files))
	for f := range Files {
		files = append(files, f)
	}
	slices.Sort(files)
	for _, f := range files {
		if err := runtimeFile(root, f, Files[f], u); err != nil {
			return err
		}
	}
	return nil
}

// workspaceMode is what the agent's workspace gets: the group may write it (the runtime's
// writes there, and the commands', which share the group); no setuid or setgid file.
func workspaceMode(dir bool, perm uint32) uint32 {
	if dir {
		return perm&0o7777 | 0o070
	}
	return perm&0o777 | 0o060
}

// privateMode is the runtime's own: its user's alone.
func privateMode(dir bool, perm uint32) uint32 {
	if dir {
		return 0o700
	}
	return perm&0o700 | 0o600
}

// readOnlyMode is the runtime's, readable by its group (the agent's commands).
func readOnlyMode(dir bool, perm uint32) uint32 {
	if dir {
		return 0o750
	}
	return perm&0o700 | 0o640
}

// runtimeDir makes rel (below the home, its parents first) a directory when missing, moving aside
// what else holds the name, then gives it and all it holds to the runtime with mode.
func runtimeDir(root int, rel string, u Users, mode func(bool, uint32) uint32, logf func(string)) error {
	dir := root
	segs := strings.Split(rel, "/")
	for i, seg := range segs {
		p := path.Join(segs[:i+1]...)
		var st unix.Stat_t
		err := unix.Fstatat(dir, seg, &st, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(err, unix.ENOENT):
			if err := unix.Mkdirat(dir, seg, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
				return &os.PathError{Op: "mkdir", Path: p, Err: err}
			}
		case err != nil:
			return &os.PathError{Op: "stat", Path: p, Err: err}
		case st.Mode&unix.S_IFMT != unix.S_IFDIR:
			aside := fmt.Sprintf("%s.moved-%d", seg, time.Now().Unix())
			if err := unix.Renameat(dir, seg, dir, aside); err != nil {
				return &os.PathError{Op: "rename", Path: p, Err: err}
			}
			logf(fmt.Sprintf("uid split: %s was not a directory; moved aside to %s", p, path.Join(path.Dir(p), aside)))
			if err := unix.Mkdirat(dir, seg, 0o700); err != nil {
				return &os.PathError{Op: "mkdir", Path: p, Err: err}
			}
		}
		if i == len(segs)-1 {
			break
		}
		next, err := unix.Openat(dir, seg, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if dir != root {
			unix.Close(dir)
		}
		if err != nil {
			return &os.PathError{Op: "open", Path: p, Err: err}
		}
		dir = next
	}
	defer func() {
		if dir != root {
			unix.Close(dir)
		}
	}()
	return fixTree(dir, segs[len(segs)-1], rel, u.RuntimeUID, u.RuntimeGID, mode)
}

// runtimeFile gives a file at the home's root to the runtime with mode; a missing one is made
// empty, anything but a regular file is an error.
func runtimeFile(root int, name string, mode uint32, u Users) error {
	fd, err := unix.Openat(root, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		fd, err = unix.Openat(root, name, unix.O_RDONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	}
	if err != nil {
		return &os.PathError{Op: "open", Path: name, Err: err}
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("%s is not a regular file", name)
	}
	if err := unix.Fchown(fd, u.RuntimeUID, u.RuntimeGID); err != nil {
		return &os.PathError{Op: "chown", Path: name, Err: err}
	}
	return unix.Fchmod(fd, mode)
}

// fixTree gives name (in dir; p is its path, for errors) and everything below it to uid:gid,
// with mode's bits on directories and regular files. Nothing is followed: a symlink is given to
// uid:gid as itself, other special files keep their mode.
func fixTree(dir int, name, p string, uid, gid int, mode func(bool, uint32) uint32) error {
	var st unix.Stat_t
	if err := unix.Fstatat(dir, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil // gone meanwhile
		}
		return &os.PathError{Op: "stat", Path: p, Err: err}
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return &os.PathError{Op: "open", Path: p, Err: err}
		}
		defer unix.Close(fd)
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return &os.PathError{Op: "chown", Path: p, Err: err}
		}
		if err := unix.Fchmod(fd, mode(true, uint32(st.Mode))); err != nil {
			return &os.PathError{Op: "chmod", Path: p, Err: err}
		}
		names, err := readNames(fd)
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := fixTree(fd, n, p+"/"+n, uid, gid, mode); err != nil {
				return err
			}
		}
		return nil
	case unix.S_IFREG:
		fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
		if err != nil {
			return &os.PathError{Op: "open", Path: p, Err: err}
		}
		defer unix.Close(fd)
		var now unix.Stat_t
		if err := unix.Fstat(fd, &now); err != nil || now.Mode&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("%s changed while the home was prepared", p)
		}
		if err := unix.Fchown(fd, uid, gid); err != nil {
			return &os.PathError{Op: "chown", Path: p, Err: err}
		}
		if err := unix.Fchmod(fd, mode(false, uint32(now.Mode))); err != nil {
			return &os.PathError{Op: "chmod", Path: p, Err: err}
		}
		return nil
	}
	if err := unix.Fchownat(dir, name, uid, gid, unix.AT_SYMLINK_NOFOLLOW); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "chown", Path: p, Err: err}
	}
	return nil
}

// readNames lists an open directory's entries.
func readNames(fd int) ([]string, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	names, err := f.Readdirnames(-1)
	slices.Sort(names)
	return names, err
}

func umask(m int) { syscall.Umask(m) }
