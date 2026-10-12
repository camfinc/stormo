package core

import (
	"os"
	"testing"
	"time"
)

func TestWindowsCoreStopSignal(t *testing.T) {
	root := t.TempDir()
	done, cleanup, err := coreStopSignal(root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !processAlive(os.Getpid()) {
		t.Fatal("current process reported dead")
	}
	if err := stopCoreProcess(t.TempDir(), os.Getpid()); err == nil {
		t.Fatal("wrong instance signalled this process")
	}
	select {
	case <-done:
		t.Fatal("wrong instance stopped core")
	default:
	}
	if err := stopCoreProcess(root, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("graceful stop was not delivered")
	}
}
