//go:build unix

package tools

import (
	"os/exec"
	"syscall"
)

// killGroup runs the command in its own process group and kills the whole group when the
// command is cancelled, so a background child cannot outlive the timeout holding the output pipe.
func killGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
