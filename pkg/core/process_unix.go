//go:build !windows

package core

import (
	"os/exec"
	"syscall"
)

func detachCore(cmd *exec.Cmd)                                 { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
func processAlive(pid int) bool                                { return syscall.Kill(pid, 0) == nil }
func stopCoreProcess(_ string, pid int) error                  { return syscall.Kill(pid, syscall.SIGTERM) }
func coreStopSignal(_ string) (<-chan struct{}, func(), error) { return nil, func() {}, nil }
