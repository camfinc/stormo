//go:build unix

package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO in a readable directory must not hang a file tool (and pin a turn slot): only regular
// files are opened.
func TestFileToolsRefuseFIFO(t *testing.T) {
	r, env := setup(t)
	if err := syscall.Mkfifo(filepath.Join(env.Home, "pipe"), 0o644); err != nil {
		t.Skip("mkfifo:", err)
	}
	for _, c := range []struct{ tool, args string }{
		{"read_file", `{"path":"pipe"}`},
		{"patch", `{"path":"pipe","old_string":"a","new_string":"b"}`},
		{"write_file", `{"path":"pipe","content":"x"}`},
	} {
		done := make(chan string, 1)
		go func() {
			out, _ := r.Call(context.Background(), env, c.tool, c.args)
			done <- out
		}()
		select {
		case out := <-done:
			if !strings.Contains(out, "not a regular file") {
				t.Errorf("%s: %s", c.tool, out)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%s hangs on a FIFO", c.tool)
		}
	}
}

// The final component is opened without following a symlink: a link swapped in after the path
// was resolved is not written through.
func TestOpenNoFollow(t *testing.T) {
	_, env := setup(t)
	target := filepath.Join(env.Home, "target.md")
	_ = os.WriteFile(target, []byte("keep"), 0o644)
	link := filepath.Join(env.Sandbox.Shared[0], "swapped.md")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(filepath.Dir(link))
	if err := writeFile(env, filepath.Join(real, "swapped.md"), []byte("x"), 0o644); err == nil {
		t.Error("wrote through a symlink")
	}
	if _, _, err := openRead(filepath.Join(real, "swapped.md"), "swapped.md"); err == nil {
		t.Error("read through a symlink")
	}
	// the flags themselves: the link is not followed even without the Lstat check before
	if f, err := os.OpenFile(link, os.O_WRONLY|oNoFollow|oNonblock, 0); err == nil {
		_ = f.Close()
		t.Error("O_NOFOLLOW open followed the link")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Errorf("target = %q", b)
	}
}
