package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// The work area (docs/needletail.md §6, §7): work/<conversation>/ in the home is where a tainted
// run's writes go (the file tools and the terminal, which then may write nothing else). It is the
// runtime's: work/ is made sticky (a command can add its own entries but never rename or remove
// the runtime's), each conversation's directory the runtime's (group-writable for the agent's
// commands), so a command cannot swap one for a symlink. The file tools reach a work directory
// only from its own conversation. No snapshot rule claims work/: it is never napped.

// WorkRoot is the work area's directory in the home.
const WorkRoot = "work"

// Modes: work/ the runtime's, entered only by its group (the boot gives it wire.WorkGID: a tainted
// run's commands), listed and written by no command; a conversation's directory group-writable
// with new entries in the group.
const (
	workRootMode = 0o710
	workConvMode = 0o2770
)

// WorkRel is a conversation's work directory, relative to the home.
func WorkRel(conversation string) string { return WorkRoot + "/" + conversation }

// inWork is rel's conversation when it is in the work area ("" with ok when it is work/ itself).
func inWork(rel string) (conv string, ok bool) {
	first, rest, _ := strings.Cut(rel, "/")
	if !strings.EqualFold(first, WorkRoot) {
		return "", false
	}
	conv, _, _ = strings.Cut(rest, "/")
	return conv, true
}

// ownWork is whether rel is in the calling conversation's own work directory.
func ownWork(env *Env, rel string) bool {
	conv, ok := inWork(rel)
	return ok && conv != "" && conv == conversationOf(env)
}

// otherWork is whether rel is in another conversation's work directory (or the work root's own
// entries a command made): the file tools never reach those.
func otherWork(env *Env, rel string) bool {
	conv, ok := inWork(rel)
	return ok && conv != conversationOf(env)
}

// EnsureWork makes the calling conversation's work directory, and its .tmp (the TMPDIR of its
// tainted commands), and returns its absolute path. Every component is reached by descriptor
// with O_NOFOLLOW; a name a command took first (a file, a symlink, a directory not the runtime's)
// is moved aside.
func EnsureWork(env *Env) (string, error) {
	conv := conversationOf(env)
	if conv == "" || strings.ContainsAny(conv, "/\\") || conv == "." || conv == ".." {
		return "", errors.New("this run has no conversation to keep a work directory for")
	}
	home, err := filepath.Abs(env.Home)
	if err != nil {
		return "", err
	}
	root, err := unix.Open(home, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: home, Err: err}
	}
	defer unix.Close(root)
	work, err := runtimeSubdir(root, WorkRoot, workRootMode, false)
	if err != nil {
		return "", err
	}
	defer unix.Close(work)
	dir, err := runtimeSubdir(work, conv, workConvMode, true)
	if err != nil {
		return "", err
	}
	defer unix.Close(dir)
	tmp, err := runtimeSubdir(dir, ".tmp", workConvMode, true)
	if err != nil {
		return "", err
	}
	unix.Close(tmp)
	return filepath.Join(home, WorkRoot, conv), nil
}

// runtimeSubdir opens name below dir (made with mode when missing), a directory reached without
// following a symlink (a directory a command made there is used as it is: the jail and the file
// tools reach it by descriptor without following one either, so a swap fails closed); with aside,
// anything else holding the name (a file, a symlink) is moved aside (<name>.moved-<time>) and the
// directory made again. The caller closes it.
func runtimeSubdir(dir int, name string, mode uint32, aside bool) (int, error) {
	for attempt := 0; attempt < 2; attempt++ {
		made := true
		if err := unix.Mkdirat(dir, name, 0o700); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return -1, &os.PathError{Op: "mkdir", Path: name, Err: err}
			}
			made = false
		}
		fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err == nil {
			var st unix.Stat_t
			if made || unix.Fstat(fd, &st) == nil && int(st.Uid) == os.Geteuid() {
				if err := unix.Fchmod(fd, mode); err != nil {
					unix.Close(fd)
					return -1, &os.PathError{Op: "chmod", Path: name, Err: err}
				}
			}
			return fd, nil
		}
		if !aside {
			return -1, fmt.Errorf("%s is not a directory: the work area is unavailable", name)
		}
		if err := unix.Renameat(dir, name, dir, fmt.Sprintf("%s.moved-%d", name, time.Now().UnixNano())); err != nil {
			return -1, &os.PathError{Op: "rename", Path: name, Err: err}
		}
	}
	return -1, fmt.Errorf("%s could not be made", name)
}
