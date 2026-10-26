//go:build linux

package jail

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ABI is the kernel's Landlock ABI version (1 and up), or an error saying why there is none: a
// kernel before 5.13 (ENOSYS), or one built without it or booted without it in its LSM list
// (EOPNOTSUPP). It needs no privilege.
func ABI() (int, error) {
	v, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	switch {
	case e == unix.ENOSYS:
		return 0, errors.New("the kernel has no Landlock (before 5.13)")
	case e == unix.EOPNOTSUPP:
		return 0, errors.New("Landlock is not enabled in this kernel (CONFIG_SECURITY_LANDLOCK, lsm=)")
	case e != 0:
		return 0, fmt.Errorf("landlock_create_ruleset: %w", e)
	case int(v) < 1:
		return 0, fmt.Errorf("landlock ABI %d", int(v))
	}
	return int(v), nil
}

// Restrict confines the calling thread, and what it executes from now on, to rules: a ruleset
// handling every right the ABI knows, one path-beneath rule per existing path (a missing
// Required one is an error), no_new_privs, then landlock_restrict_self. Landlock and no_new_privs
// are per thread: the caller locks its goroutine to the thread and execs from it.
func Restrict(rules []Rule, abi int) error {
	attr := unix.LandlockRulesetAttr{Access_fs: Handled(abi)}
	fd, _, e := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	runtime.KeepAlive(&attr)
	if e != 0 {
		return fmt.Errorf("landlock_create_ruleset: %w", e)
	}
	defer unix.Close(int(fd))
	for _, r := range rules {
		if err := addRule(int(fd), r, abi); err != nil {
			return err
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", err)
	}
	if _, _, e := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); e != 0 {
		return fmt.Errorf("landlock_restrict_self: %w", e)
	}
	return nil
}

// addRule adds r beneath its path, opened O_PATH (system paths may be symlinks: /lib64,
// /etc/localtime; the rule binds what they resolve to).
func addRule(ruleset int, r Rule, abi int) error {
	flags := unix.O_PATH | unix.O_CLOEXEC
	if r.NoFollow {
		flags |= unix.O_NOFOLLOW
	}
	pfd, err := unix.Open(r.Path, flags, 0)
	if err != nil {
		if !r.Required && (errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR)) {
			return nil
		}
		return &os.PathError{Op: "open", Path: r.Path, Err: err}
	}
	defer unix.Close(pfd)
	var st unix.Stat_t
	if err := unix.Fstat(pfd, &st); err != nil {
		return &os.PathError{Op: "stat", Path: r.Path, Err: err}
	}
	dir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	if r.Required && !dir {
		return fmt.Errorf("%s is not a directory", r.Path)
	}
	access := r.Grant(abi, dir)
	if access == 0 {
		return nil
	}
	pb := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(pfd)}
	_, _, e := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&pb)), 0, 0, 0)
	runtime.KeepAlive(&pb)
	if e != 0 {
		return fmt.Errorf("landlock_add_rule %s: %w", r.Path, e)
	}
	return nil
}

// Main is `needletail jail --home H [--shared S]… -- <path> <argv…>`: on its own thread it makes
// the commands' temporary directory if it is missing (TmpDir, as the command's user), restricts
// itself to spec's rules and execs path with argv and its own environment. It returns only when
// that fails: ExitJail (nothing ran) or ExitExec.
func Main(args []string, stderr io.Writer) int {
	spec, path, argv, err := parse(args)
	if err != nil {
		fmt.Fprintf(stderr, "needletail jail: %v\n", err)
		return ExitJail
	}
	runtime.LockOSThread() // never unlocked: the thread execs or the process exits
	abi, err := ABI()
	if err != nil {
		fmt.Fprintf(stderr, "needletail jail: %v\n", err)
		return ExitJail
	}
	if spec.ReadOnlyHome && abi < 3 {
		fmt.Fprintln(stderr, "needletail jail: quarantine requires Landlock ABI 3 or newer to deny truncation")
		return ExitJail
	}
	// best effort: the runtime makes it at boot; a command may have removed it since
	if err := os.Mkdir(filepath.Join(spec.Home, TmpDir), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		fmt.Fprintf(stderr, "needletail jail: %v\n", err)
	}
	if err := Restrict(spec.Rules(), abi); err != nil {
		fmt.Fprintf(stderr, "needletail jail: %v\n", err)
		return ExitJail
	}
	err = syscall.Exec(path, argv, os.Environ())
	fmt.Fprintf(stderr, "needletail jail: exec %s: %v\n", path, err)
	return ExitExec
}
