package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A file the runtime reads from where the agent's commands write (a handler, a tool, a skill's
// SKILL.md, a schedule's .luau script) is read by descriptor: a FIFO or a device swapped in after
// a check never blocks the read nor feeds it without end, a symlink anywhere below the root is
// refused, and no more than max+1 bytes are read.
func TestReadFileBelow(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "skills", "a", "handlers"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(root, "skills", "a", "handlers", "ok.luau")
	if err := os.WriteFile(ok, []byte("return nil"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFileBelow(root, "skills/a/handlers/ok.luau", 100); err != nil || string(b) != "return nil" {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := ReadFileBelow(root, "skills/a/handlers/ok.luau", 5); err == nil || !strings.Contains(err.Error(), "over 5 bytes") {
		t.Errorf("oversize: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "skills", "a", "handlers", "fifo.luau"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/zero", filepath.Join(root, "skills", "a", "handlers", "zero.luau")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "skills", "a"), filepath.Join(root, "skills", "b")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := ReadFileBelow(root, "skills/a/handlers/fifo.luau", 100); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("fifo: %v", err)
		}
		if _, err := ReadFileBelow(root, "skills/a/handlers/zero.luau", 100); !errors.Is(err, ErrLink) {
			t.Errorf("a symlink to /dev/zero: %v", err)
		}
		if _, err := ReadFileBelow(root, "skills/b/handlers/ok.luau", 100); !errors.Is(err, ErrLink) {
			t.Errorf("through a symlinked directory: %v", err)
		}
		if _, err := ReadFileBelow(root, "skills/a/handlers/none.luau", 100); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("missing: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a read blocked")
	}
}
