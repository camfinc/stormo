//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// command prepares a command the agent runs: in its own process group, killed whole when the
// command is cancelled (so a background child cannot outlive the timeout holding the output
// pipe), and, with the uid split, as the agent's user with no supplementary group. The runtime
// keeps CAP_SETUID/CAP_SETGID to start it so and CAP_KILL to kill its group; the command itself
// starts with no capability.
func command(cmd *exec.Cmd, agent *User) {
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
