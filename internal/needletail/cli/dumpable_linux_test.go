//go:build linux

package cli

import (
	"syscall"
	"testing"
)

func TestUndumpable(t *testing.T) {
	if err := undumpable(); err != nil {
		t.Fatal(err)
	}
	r, _, e := syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_GET_DUMPABLE, 0, 0)
	if e != 0 || r != 0 {
		t.Errorf("dumpable = %d (%v)", r, e)
	}
}
