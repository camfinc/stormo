//go:build linux

package privsep

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Capabilities the runtime keeps: SETUID and SETGID to start the agent's commands as the exec
// user, KILL to stop their process groups at a timeout. Nothing else, in no set but effective
// and permitted: the commands get none (they are not root and inherit no ambient capability, and
// no-new-privileges keeps a file's from applying).
const (
	capKill   = 5
	capSetgid = 6
	capSetuid = 7
	// Keep is the capability mask the runtime runs with.
	Keep = 1<<capKill | 1<<capSetgid | 1<<capSetuid
)

const linuxCapabilityVersion3 = 0x20080522

type capHeader struct {
	version uint32
	pid     int32
}

type capData struct {
	effective, permitted, inheritable uint32
}

// Drop switches the process, every thread of it, from root to u.RuntimeUID:u.RuntimeGID with no
// supplementary groups, keeping only Keep (effective and permitted), and checks the result in
// /proc. Every credential is per thread on Linux and the Go runtime has several, so each step
// is made on all of them (syscall.AllThreadsSyscall; the binary is built without cgo).
func Drop(u Users) error {
	// The bounding set loses everything else first (best effort: it needs CAP_SETPCAP).
	last := lastCap()
	for c := 0; c <= last; c++ {
		if Keep&(1<<c) != 0 {
			continue
		}
		if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_CAPBSET_DROP, uintptr(c), 0); e != 0 {
			if e == syscall.EINVAL {
				break
			}
			if e == syscall.EPERM {
				break // no CAP_SETPCAP: the commands get no capabilities anyway
			}
			return fmt.Errorf("prctl(PR_CAPBSET_DROP, %d): %w", c, e)
		}
	}
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_SET_KEEPCAPS, 1, 0); e != 0 {
		return fmt.Errorf("prctl(PR_SET_KEEPCAPS): %w", e)
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setresgid(u.RuntimeGID, u.RuntimeGID, u.RuntimeGID); err != nil {
		return fmt.Errorf("setresgid: %w", err)
	}
	if err := syscall.Setresuid(u.RuntimeUID, u.RuntimeUID, u.RuntimeUID); err != nil {
		return fmt.Errorf("setresuid: %w", err)
	}
	// The uid change cleared the effective set and kept the permitted (KEEPCAPS): keep Keep in
	// both, nothing inheritable.
	hdr := &capHeader{version: linuxCapabilityVersion3}
	data := &[2]capData{{effective: Keep, permitted: Keep}}
	_, _, e := syscall.AllThreadsSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(hdr)), uintptr(unsafe.Pointer(data)), 0)
	runtime.KeepAlive(hdr)
	runtime.KeepAlive(data)
	if e != 0 {
		return fmt.Errorf("capset: %w", e)
	}
	if _, _, e := syscall.AllThreadsSyscall(syscall.SYS_PRCTL, unix.PR_SET_KEEPCAPS, 0, 0); e != 0 {
		return fmt.Errorf("prctl(PR_SET_KEEPCAPS, 0): %w", e)
	}
	if _, _, e := syscall.AllThreadsSyscall6(syscall.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0, 0); e != 0 && e != syscall.EINVAL {
		return fmt.Errorf("prctl(PR_CAP_AMBIENT_CLEAR_ALL): %w", e)
	}
	// non-dumpable: the runtime's /proc entries (environ, mem, …) are root's (the uid change
	// already made it so; said explicitly)
	if _, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_DUMPABLE, 0, 0); e != 0 {
		return fmt.Errorf("prctl(PR_SET_DUMPABLE): %w", e)
	}
	return Verify(u)
}

// Verify checks every thread of the process: the runtime's uid and gid in all their slots, no
// supplementary group, exactly Keep effective and permitted, nothing inheritable or ambient.
func Verify(u Users) error {
	tasks, err := filepath.Glob("/proc/self/task/*/status")
	if err != nil || len(tasks) == 0 {
		return fmt.Errorf("no /proc/self/task: %v", err)
	}
	want := map[string]string{
		"Uid":    strings.Repeat(strconv.Itoa(u.RuntimeUID)+" ", 4),
		"Gid":    strings.Repeat(strconv.Itoa(u.RuntimeGID)+" ", 4),
		"Groups": "",
		"CapEff": fmt.Sprintf("%016x", Keep),
		"CapPrm": fmt.Sprintf("%016x", Keep),
		"CapInh": fmt.Sprintf("%016x", 0),
		"CapAmb": fmt.Sprintf("%016x", 0),
	}
	for _, task := range tasks {
		f, err := os.Open(task)
		if err != nil {
			continue // the thread ended
		}
		got := map[string]string{}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if ok {
				got[k] = strings.Join(strings.Fields(v), " ")
			}
		}
		f.Close()
		for k, w := range want {
			w = strings.TrimSpace(w)
			if g, ok := got[k]; ok && g != w || !ok && k != "CapAmb" {
				return fmt.Errorf("%s: %s is %q, want %q", task, k, got[k], w)
			}
		}
	}
	return nil
}

// lastCap is the kernel's highest capability number.
func lastCap() int {
	b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap")
	if n, e := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && e == nil && n > 0 {
		return n
	}
	return 40
}
