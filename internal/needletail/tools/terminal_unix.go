//go:build unix

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// start prepares a command the agent runs: in its own process group, killed whole when the
// command is cancelled (so a background child cannot outlive the timeout holding the output
// pipe), and, with the uid split, as the agent's user with no supplementary group. The runtime
// keeps CAP_SETUID/CAP_SETGID to start it so and CAP_KILL to kill its group; the command itself
// starts with no capability.
func start(cmd *exec.Cmd, agent *User) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if agent != nil {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: agent.UID, Gid: agent.GID, Groups: []uint32{}}
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killGroup signals the process group led by pid: SIGKILL when hard, else SIGTERM.
func killGroup(pid int, hard bool) error {
	sig := syscall.SIGTERM
	if hard {
		sig = syscall.SIGKILL
	}
	return syscall.Kill(-pid, sig)
}

// signalOf names the signal that ended a process ("" when it exited).
func signalOf(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
