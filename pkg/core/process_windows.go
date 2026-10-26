package core

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// The detached core writes to its log, owns no console, and outlives the UI/CLI.
func detachCore(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
}

func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}

// A per-process, per-instance event lets core down request graceful Close without
// attaching to a console or forcibly killing a PID that Windows may have reused.
// The default Windows DACL applies; the event is scoped to this logon session.
func stopEventName(root string, pid int) *uint16 {
	root, _ = filepath.Abs(root)
	if real, err := filepath.EvalSymlinks(root); err == nil {
		root = real
	}
	sum := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(root))))
	name, _ := windows.UTF16PtrFromString(fmt.Sprintf(`Local\Stormo-Core-%x-%d`, sum[:16], pid))
	return name
}

func stopCoreProcess(root string, pid int) error {
	h, err := windows.OpenEvent(windows.EVENT_MODIFY_STATE, false, stopEventName(root, pid))
	if err != nil {
		return fmt.Errorf("core stop event unavailable; process left unchanged: %w", err)
	}
	defer windows.CloseHandle(h)
	return windows.SetEvent(h)
}

func coreStopSignal(root string) (<-chan struct{}, func(), error) {
	h, err := windows.CreateEvent(nil, 1, 0, stopEventName(root, os.Getpid()))
	if err != nil {
		if h != 0 {
			windows.CloseHandle(h)
		}
		return nil, nil, fmt.Errorf("create core stop event: %w", err)
	}
	done := make(chan struct{})
	go func() { windows.WaitForSingleObject(h, windows.INFINITE); close(done) }()
	return done, func() { windows.SetEvent(h); <-done; windows.CloseHandle(h) }, nil
}
