//go:build linux

package cli

import "syscall"

// undumpable sets PR_SET_DUMPABLE 0: /proc/<pid>/environ, mem and the like of this process become
// root's, so a terminal command running as the same uid cannot read the secrets the process was
// started with (the container's PID 1 env). Children are dumpable again after their execve.
func undumpable() error {
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); e != 0 {
		return e
	}
	return nil
}
