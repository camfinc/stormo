package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
)

func setup(t *testing.T) (*Registry, *Env) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	group := filepath.Join(base, "shared", "group")
	for _, d := range []string{home, group, filepath.Join(home, "sessions"), filepath.Join(home, "memories")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := &Env{
		Home:           home,
		Sandbox:        sandbox.Sandbox{Home: home, Shared: []string{group}},
		CommandTimeout: 5 * time.Second,
		Environ: func() []string {
			return []string{"PATH=" + os.Getenv("PATH"), "HOME=/root", "CRM_API_TOKEN=acme_abcdefgh123"}
		},
		Memory: &memory.Store{Home: home, Limits: map[string]int{"memory": 100, "user": 100}},
		Redact: func(s string) string { return strings.ReplaceAll(s, "acme_abcdefgh123", "[REDACTED]") },
	}
	return NewRegistry(Builtins()...), env
}

func call(t *testing.T, r *Registry, env *Env, name string, args any) (map[string]any, bool) {
	t.Helper()
	b, _ := json.Marshal(args)
	out, failed := r.Call(context.Background(), env, name, string(b))
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("%s: result is not JSON: %s", name, out)
	}
	return m, failed
}

func TestTerminal(t *testing.T) {
	r, env := setup(t)
	m, failed := call(t, r, env, "terminal", map[string]any{"command": `echo "home=$HOME"; pwd; echo token=$CRM_API_TOKEN; echo err >&2; exit 3`})
	out, _ := m["output"].(string)
	real, _ := filepath.EvalSymlinks(env.Home)
	if failed || m["exit_code"] != float64(3) || !strings.Contains(out, "home="+env.Home) || !strings.Contains(out, real) || !strings.Contains(out, "err") {
		t.Errorf("terminal = %v", m)
	}
	if strings.Contains(out, "acme_abcdefgh123") || !strings.Contains(out, "token=[REDACTED]") {
		t.Errorf("not redacted: %s", out)
	}
	// a background child holding the pipe does not outlive the timeout
	start := time.Now()
	m, _ = call(t, r, env, "terminal", map[string]any{"command": "sleep 30 & sleep 30; echo never", "timeout": 1})
	if time.Since(start) > 6*time.Second || m["exit_code"] != float64(-1) || !strings.Contains(m["error"].(string), "timed out") {
		t.Errorf("timeout = %v after %s", m, time.Since(start))
	}
	m, _ = call(t, r, env, "terminal", map[string]any{"command": "head -c 200000 /dev/zero | tr '\\0' x"})
	if out := m["output"].(string); len(out) > outputHead+outputTail+200 || !strings.Contains(out, "elided") {
		t.Errorf("cap: %d bytes", len(out))
	}
	if m, failed := call(t, r, env, "terminal", map[string]any{"command": "pwd", "workdir": "/etc"}); !failed || !strings.Contains(m["error"].(string), "outside") {
		t.Errorf("workdir escape = %v", m)
	}
}

func TestFileTools(t *testing.T) {
	r, env := setup(t)
	var notes []string
	env.SharedWrite = func(_ context.Context, p string) { notes = append(notes, p) }
	if m, failed := call(t, r, env, "write_file", map[string]any{"path": "notes/a.md", "content": "one\ntwo\nthree\n"}); failed || m["bytes_written"] != float64(14) || m["path"] != "notes/a.md" {
		t.Errorf("write = %v", m)
	}
	m, _ := call(t, r, env, "read_file", map[string]any{"path": "notes/a.md", "offset": 2, "limit": 1})
	if m["content"] != "     2|two\n" || m["total_lines"] != float64(3) || m["truncated"] != true || m["next_offset"] != float64(3) {
		t.Errorf("read = %v", m)
	}
	if m, failed := call(t, r, env, "patch", map[string]any{"path": "notes/a.md", "old_string": "two", "new_string": "2"}); failed || m["replacements"] != float64(1) {
		t.Errorf("patch = %v", m)
	}
	_, _ = call(t, r, env, "write_file", map[string]any{"path": "notes/b.md", "content": "x x x"})
	if m, failed := call(t, r, env, "patch", map[string]any{"path": "notes/b.md", "old_string": "x", "new_string": "y"}); !failed || !strings.Contains(m["error"].(string), "3 times") {
		t.Errorf("ambiguous patch = %v", m)
	}
	if m, failed := call(t, r, env, "patch", map[string]any{"path": "notes/b.md", "old_string": "x", "new_string": "y", "replace_all": true}); failed || m["replacements"] != float64(3) {
		t.Errorf("replace_all = %v", m)
	}
	// shared writes are allowed and noted
	shared := filepath.Join(env.Sandbox.Shared[0], "report.md")
	if _, failed := call(t, r, env, "write_file", map[string]any{"path": shared, "content": "hi"}); failed || len(notes) != 1 {
		t.Errorf("shared write: notes %v", notes)
	}
	// refused: escapes, protected runtime files, directories
	for _, p := range []string{"../outside.md", "/etc/passwd", "sessions/x.jsonl", "needletail.yaml", "state/needletail/idem.json", "notes"} {
		if m, failed := call(t, r, env, "write_file", map[string]any{"path": p, "content": "x"}); !failed {
			t.Errorf("%s written: %v", p, m)
		}
	}
	if _, failed := call(t, r, env, "read_file", map[string]any{"path": "/etc/hosts"}); !failed {
		t.Error("read outside")
	}
	_ = os.WriteFile(filepath.Join(env.Home, "bin.dat"), []byte{1, 0, 2}, 0o644)
	if m, failed := call(t, r, env, "read_file", map[string]any{"path": "bin.dat"}); !failed || !strings.Contains(m["error"].(string), "binary") {
		t.Errorf("binary = %v", m)
	}
	_ = os.WriteFile(filepath.Join(env.Home, "sessions", "s.jsonl"), []byte("three\n"), 0o600)

	m, _ = call(t, r, env, "search_files", map[string]any{"pattern": "thr+ee"})
	if ms := m["matches"].([]any); len(ms) != 1 || ms[0] != "notes/a.md:3: three" {
		t.Errorf("content search = %v", m)
	}
	m, _ = call(t, r, env, "search_files", map[string]any{"pattern": "*.md", "target": "files", "path": "notes"})
	if m["count"] != float64(2) {
		t.Errorf("file search = %v", m)
	}
	m, _ = call(t, r, env, "search_files", map[string]any{"pattern": "x", "target": "files", "limit": 1, "path": "."})
	if m["count"] != float64(0) {
		t.Errorf("substring = %v", m)
	}
	if _, failed := call(t, r, env, "search_files", map[string]any{"pattern": "(", "target": "content"}); !failed {
		t.Error("bad regexp accepted")
	}
}

func TestMemoryAndErrors(t *testing.T) {
	r, env := setup(t)
	if m, failed := call(t, r, env, "memory", map[string]any{"action": "add", "target": "user", "content": "Prefers short answers."}); failed || m["usage"] != "22/100" {
		t.Errorf("memory add = %v", m)
	}
	if m, failed := call(t, r, env, "memory", map[string]any{"action": "add", "target": "user", "content": strings.Repeat("long ", 30)}); !failed || !strings.Contains(m["error"].(string), "of 100") {
		t.Errorf("over budget = %v", m)
	}
	if m, failed := call(t, r, env, "memory", map[string]any{"action": "remove", "target": "user", "old_text": "short"}); failed || m["entries"] != float64(0) {
		t.Errorf("remove = %v", m)
	}
	if m, failed := call(t, r, env, "memory", map[string]any{"action": "forget", "target": "user"}); !failed {
		t.Errorf("bad action = %v", m)
	}
	if out, failed := r.Call(context.Background(), env, "nope", "{}"); !failed || !strings.Contains(out, "unknown tool") {
		t.Errorf("unknown = %s", out)
	}
	if out, failed := r.Call(context.Background(), env, "terminal", "{not json"); !failed || !strings.Contains(out, "not valid JSON") {
		t.Errorf("bad json = %s", out)
	}
	panicky := NewRegistry(Tool{Name: "boom", Run: func(context.Context, *Env, json.RawMessage) (any, error) { panic("bad") }})
	if out, failed := panicky.Call(context.Background(), env, "boom", ""); !failed || !strings.Contains(out, "crashed") {
		t.Errorf("panic = %s", out)
	}
	if names := r.Names(); len(names) != 6 || names[0] != "memory" {
		t.Errorf("names = %v", names)
	}
	if f := r.Filter([]string{"terminal", "ghost"}); len(f) != 1 || f[0].Name != "terminal" {
		t.Errorf("filter = %v", f)
	}
}
