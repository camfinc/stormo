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
	if _, _, err := openRead(env, filepath.Join(real, "swapped.md"), "swapped.md"); err == nil {
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

// Every component of a read is opened without following a symlink, not only the last: a
// directory the agent's commands swap for a symlink to the runtime's sessions/ after the path
// was resolved is not read through (search_files would otherwise show a conversation).
func TestReadNoFollowAncestor(t *testing.T) {
	_, env := setup(t)
	put := func(rel, body string) {
		p := filepath.Join(env.Home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	put("work/dir/notes.jsonl", "mine")
	put("sessions/notes.jsonl", "a client's conversation")
	real, err := env.Sandbox.Resolve("work/dir/notes.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	// the race: once resolved, work/dir becomes a link to sessions/
	if err := os.Rename(filepath.Join(env.Home, "work", "dir"), filepath.Join(env.Home, "work", "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(env.Home, "sessions"), filepath.Join(env.Home, "work", "dir")); err != nil {
		t.Fatal(err)
	}
	if f, _, err := openRead(env, real, "work/dir/notes.jsonl"); err == nil {
		b := make([]byte, 64)
		n, _ := f.Read(b)
		_ = f.Close()
		t.Errorf("read through a swapped ancestor: %q", b[:n])
	}
}
