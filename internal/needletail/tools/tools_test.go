package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/skills"
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
		Memory:        &memory.Store{Home: home, Limits: map[string]int{"memory": 100, "user": 100}},
		Redact:        func(s string) string { return strings.ReplaceAll(s, "acme_abcdefgh123", "[REDACTED]") },
		LongestSecret: func() int { return len("acme_abcdefgh123") },
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
	if names := r.Names(); !slices.Equal(names, []string{"memory", "patch", "read_file", "search_files", "skill_manage", "skill_view", "skills_list", "terminal", "write_file"}) {
		t.Errorf("names = %v", names)
	}
	if f := r.Filter([]string{"terminal", "ghost"}); len(f) != 1 || f[0].Name != "terminal" {
		t.Errorf("filter = %v", f)
	}
}

// A secret whose value JSON escapes (quotes, &, <, >, backslash, control characters) is masked
// before the result is encoded, so no escaped form of it reaches the model or a session file.
func TestRedactBeforeEncoding(t *testing.T) {
	r, env := setup(t)
	weird := `pa"ss<w>&ord\x` + "\t9"
	env.Environ = func() []string { return []string{"PATH=" + os.Getenv("PATH"), "WEIRD_TOKEN=" + weird} }
	env.Redact = func(s string) string { return strings.ReplaceAll(s, weird, "[REDACTED]") }
	out, _ := r.Call(context.Background(), env, "terminal", `{"command":"printf '%s' \"$WEIRD_TOKEN\""}`)
	raw, _ := json.Marshal(weird)
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(weird)
	for _, form := range []string{weird, strings.Trim(string(raw), `"`), strings.Trim(strings.TrimSpace(b.String()), `"`), `pa\"ss`} {
		if strings.Contains(out, form) {
			t.Errorf("result carries %q: %s", form, out)
		}
	}
	if !strings.Contains(out, "[REDACTED]") {
		t.Errorf("not masked: %s", out)
	}
	// an error message too
	_ = os.WriteFile(filepath.Join(env.Home, weird[:2]+".txt"), nil, 0o644)
	out, _ = r.Call(context.Background(), env, "read_file", `{"path":`+string(raw)+`}`)
	if strings.Contains(out, `pa\"ss`) || !strings.Contains(out, "[REDACTED]") {
		t.Errorf("error = %s", out)
	}
}

// Capping long output never leaves part of a secret at the cut: a secret straddling the end of
// the kept head or the start of the kept tail is masked whole or dropped whole.
func TestTerminalCapKeepsSecretsWhole(t *testing.T) {
	r, env := setup(t)
	for _, c := range []struct{ cmd, part string }{
		{fmt.Sprintf(`head -c %d /dev/zero | tr '\0' a; printf %%s "$CRM_API_TOKEN"; head -c 40000 /dev/zero | tr '\0' b`, outputHead-6), "acme_a"},
		{fmt.Sprintf(`head -c 50000 /dev/zero | tr '\0' a; printf %%s "$CRM_API_TOKEN"; head -c %d /dev/zero | tr '\0' b`, outputTail-8), "gh123"},
	} {
		m, _ := call(t, r, env, "terminal", map[string]any{"command": c.cmd})
		out := m["output"].(string)
		if strings.Contains(out, c.part) || !strings.Contains(out, "elided") {
			t.Errorf("%s: output keeps %q (%d bytes)", c.cmd, c.part, len(out))
		}
	}
}

// A symlink planted in the shared space that points into the home does not let a shared write
// land in the home.
func TestSharedSymlinkIntoHome(t *testing.T) {
	r, env := setup(t)
	_ = os.MkdirAll(filepath.Join(env.Home, "skills", "crm"), 0o755)
	if err := os.Symlink(filepath.Join(env.Home, "skills"), filepath.Join(env.Sandbox.Shared[0], "x")); err != nil {
		t.Fatal(err)
	}
	m, failed := call(t, r, env, "write_file", map[string]any{"path": filepath.Join(env.Sandbox.Shared[0], "x", "crm", "SKILL.md"), "content": "evil"})
	if !failed || !strings.Contains(m["error"].(string), "outside") {
		t.Errorf("write = %v", m)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "skills", "crm", "SKILL.md")); !os.IsNotExist(err) {
		t.Error("the shared write landed in the home")
	}
}

// read_file holds at most one capped line of a file in memory, however long the line; patch
// refuses a file over its size cap.
func TestFileToolsBounded(t *testing.T) {
	r, env := setup(t)
	big := filepath.Join(env.Home, "one-line.txt")
	f, _ := os.Create(big)
	chunk := []byte(strings.Repeat("a", 1<<20))
	for i := 0; i < 48; i++ {
		_, _ = f.Write(chunk)
	}
	_, _ = f.Write([]byte("\nsecond line\n"))
	_ = f.Close()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	m, failed := call(t, r, env, "read_file", map[string]any{"path": "one-line.txt"})
	runtime.ReadMemStats(&after)
	if failed || m["total_lines"] != float64(2) || !strings.Contains(m["content"].(string), "     2|second line") {
		t.Errorf("read = %v", m["error"])
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 8<<20 {
		t.Errorf("read_file allocated %d MiB for a 48 MiB line", got>>20)
	}
	m, failed = call(t, r, env, "patch", map[string]any{"path": "one-line.txt", "old_string": "second", "new_string": "2nd"})
	if !failed || !strings.Contains(m["error"].(string), "too large") {
		t.Errorf("patch = %v", m)
	}
}

// search_files does not search the runtime's own directories, also when asked for one directly.
func TestSearchRuntimeDirs(t *testing.T) {
	r, env := setup(t)
	_ = os.MkdirAll(filepath.Join(env.Home, "sessions", "sub"), 0o700)
	_ = os.MkdirAll(filepath.Join(env.Home, "logs"), 0o700)
	_ = os.WriteFile(filepath.Join(env.Home, "sessions", "sub", "s.jsonl"), []byte("private talk\n"), 0o600)
	_ = os.WriteFile(filepath.Join(env.Home, "logs", "x.log"), []byte("private talk\n"), 0o600)
	for _, p := range []string{"sessions", "sessions/sub", "logs", "state/needletail"} {
		_ = os.MkdirAll(filepath.Join(env.Home, p), 0o700)
		if m, failed := call(t, r, env, "search_files", map[string]any{"pattern": "private", "path": p}); !failed {
			t.Errorf("%s searched: %v", p, m)
		}
	}
	if m, _ := call(t, r, env, "search_files", map[string]any{"pattern": "private"}); m["count"] != float64(0) {
		t.Errorf("home search = %v", m)
	}
}

// A command that leaves a background process running (a server it starts on purpose) returns its
// output and exit code, with a note; the process is not killed.
func TestTerminalBackgroundChild(t *testing.T) {
	r, env := setup(t)
	marker := filepath.Join(env.Home, "bg-done")
	start := time.Now()
	m, failed := call(t, r, env, "terminal", map[string]any{"command": "(sleep 6; touch " + marker + ") & echo started"})
	if failed || m["exit_code"] != float64(0) || !strings.Contains(m["output"].(string), "started") || !strings.Contains(fmt.Sprint(m["note"]), "background") {
		t.Errorf("terminal = %v", m)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %s", took)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("the background process was killed")
}

// Tools join a registry while turns read it (the core's tools arrive after boot).
func TestRegistryAddWhileInUse(t *testing.T) {
	r := NewRegistry(Builtins()...)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			r.Add(Tool{Name: fmt.Sprintf("mcp_core_t%d", i), Run: func(context.Context, *Env, json.RawMessage) (any, error) { return map[string]int{"n": 1}, nil }})
		}
	}()
	for i := 0; i < 50; i++ {
		_ = r.Filter(nil)
		_, _ = r.Call(context.Background(), &Env{}, "mcp_core_t0", "{}")
	}
	<-done
	if out, failed := r.Call(context.Background(), &Env{}, "mcp_core_t49", "{}"); failed || out != `{"n":1}` {
		t.Errorf("added tool: %s", out)
	}
	if len(r.Names()) != len(Builtins())+50 {
		t.Errorf("names %d", len(r.Names()))
	}
}

// The skill tools: list, view (with ${SKILL_DIR} resolved), the agent's own changes, the
// terminal run from a skill, and the usage they leave in skills/.usage.json.
func TestSkillTools(t *testing.T) {
	r, env := setup(t)
	env.Session = "s1"
	env.Skills = &skills.Library{Home: env.Home, Generated: []string{"acme-knowledge"}, Check: SecretCheck(env.Redact)}
	for rel, body := range map[string]string{
		"skills/crm/crm-api/SKILL.md":         "---\nname: crm-api\ndescription: Query deals.\n---\nRun bash ${SKILL_DIR}/scripts/deals.sh\n",
		"skills/crm/crm-api/scripts/deals.sh": "echo \"deals from $SKILL_DIR\"\n",
		"skills/acme-knowledge/SKILL.md":      "---\nname: acme-knowledge\ndescription: Knowledge.\n---\n",
	} {
		p := filepath.Join(env.Home, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(body), 0o644)
	}
	dir := filepath.Join(env.Home, "skills", "crm", "crm-api")

	m, failed := call(t, r, env, "skills_list", map[string]any{})
	if failed || m["count"] != float64(2) {
		t.Errorf("list = %v", m)
	}
	m, failed = call(t, r, env, "skill_view", map[string]any{"name": "crm-api"})
	if failed || !strings.Contains(m["content"].(string), "bash "+dir+"/scripts/deals.sh") || m["skill_dir"] != dir {
		t.Errorf("view = %v", m)
	}
	// the session viewed crm-api: a command using SKILL_DIR runs from it
	m, failed = call(t, r, env, "terminal", map[string]any{"command": `bash "$SKILL_DIR/scripts/deals.sh"`})
	if failed || m["output"] != "deals from "+dir+"\n" {
		t.Errorf("terminal from the viewed skill = %v", m)
	}
	m, _ = call(t, r, env, "terminal", map[string]any{"command": "echo [$SKILL_DIR]"})
	if !strings.Contains(m["output"].(string), dir) {
		t.Errorf("SKILL_DIR = %v", m)
	}
	// another session has viewed nothing
	env2 := *env
	env2.Session = "s2"
	if m, failed := call(t, r, &env2, "terminal", map[string]any{"command": "echo $SKILL_DIR"}); !failed || !strings.Contains(m["error"].(string), "pass skill") {
		t.Errorf("no skill = %v", m)
	}
	if m, failed := call(t, r, &env2, "terminal", map[string]any{"command": "echo [$SKILL_DIR]", "skill": "crm/crm-api"}); failed || !strings.Contains(m["output"].(string), dir) {
		t.Errorf("explicit skill = %v", m)
	}
	if m, failed := call(t, r, &env2, "terminal", map[string]any{"command": "bash skills/crm/crm-api/scripts/deals.sh"}); failed || !strings.Contains(m["output"].(string), dir) {
		t.Errorf("by path = %v", m)
	}
	if m, failed := call(t, r, &env2, "terminal", map[string]any{"command": "echo plain"}); failed || m["output"] != "plain\n" {
		t.Errorf("plain = %v", m)
	}
	// naming SKILL_DIR is not using it: only $SKILL_DIR or ${SKILL_DIR} needs a skill
	if m, failed := call(t, r, &env2, "terminal", map[string]any{"command": "grep -rln SKILL_DIR skills; echo $SKILL_DIRS done"}); failed || !strings.Contains(m["output"].(string), "deals.sh") {
		t.Errorf("grep SKILL_DIR = %v", m)
	}
	if m, failed := call(t, r, &env2, "terminal", map[string]any{"command": "echo ${SKILL_DIR}"}); !failed || !strings.Contains(m["error"].(string), "pass skill") {
		t.Errorf("braced SKILL_DIR = %v", m)
	}

	md := "---\nname: deal-digest\ndescription: Digest the week's deals.\n---\nRun ${SKILL_DIR}/digest.sh\n"
	m, failed = call(t, r, env, "skill_manage", map[string]any{"action": "create", "name": "deal-digest", "category": "crm", "content": md})
	if failed || m["dir"] != "crm/deal-digest" || m["path"] != "skills/crm/deal-digest/SKILL.md" {
		t.Errorf("create = %v", m)
	}
	if m, failed := call(t, r, env, "skill_manage", map[string]any{"action": "write_file", "name": "deal-digest", "file_path": "digest.sh", "file_content": "echo token=acme_abcdefgh123"}); !failed || !strings.Contains(m["error"].(string), "secret") {
		t.Errorf("secret written = %v", m)
	}
	// a secret anywhere in the call (a path, a name) or assembled across two patches is refused
	for _, args := range []map[string]any{
		{"action": "write_file", "name": "deal-digest", "file_path": "acme_abcdefgh123.md", "file_content": "x"},
		{"action": "create", "name": "acme_abcdefgh123", "content": md},
		{"action": "create", "name": "other", "category": "acme_abcdefgh123", "content": md},
	} {
		if m, failed := call(t, r, env, "skill_manage", args); !failed || !strings.Contains(m["error"].(string), "secret") {
			t.Errorf("secret in %v = %v", args, m)
		}
	}
	if _, failed := call(t, r, env, "skill_manage", map[string]any{"action": "write_file", "name": "deal-digest", "file_path": "notes.md", "file_content": "token=acme_abcd\n"}); failed {
		t.Error("write notes.md")
	}
	if m, failed := call(t, r, env, "skill_manage", map[string]any{"action": "patch", "name": "deal-digest", "file_path": "notes.md", "old_string": "abcd\n", "new_string": "abcdefgh123\n"}); !failed || !strings.Contains(m["error"].(string), "secret") {
		t.Errorf("split secret = %v", m)
	}
	if m, failed := call(t, r, env, "skill_manage", map[string]any{"action": "patch", "name": "acme-knowledge", "old_string": "Knowledge", "new_string": "x"}); !failed || !strings.Contains(m["error"].(string), "generated") {
		t.Errorf("generated patched = %v", m)
	}
	if m, failed := call(t, r, env, "skill_manage", map[string]any{"action": "write_file", "name": "deal-digest", "file_path": "../crm-api/SKILL.md", "file_content": "x"}); !failed {
		t.Errorf("traversal = %v", m)
	}
	if m, failed := call(t, r, env, "skill_manage", map[string]any{"action": "rename", "name": "deal-digest"}); !failed {
		t.Errorf("unknown action = %v", m)
	}
	// read_file of a SKILL.md counts as a view
	if _, failed := call(t, r, env, "read_file", map[string]any{"path": "skills/crm/deal-digest/SKILL.md"}); failed {
		t.Error("read_file")
	}
	u, _ := env.Skills.Usage()
	if e := u["crm-api"]; e["view_count"] != float64(1) || e["use_count"] != float64(5) {
		t.Errorf("crm-api usage = %v", e)
	}
	if e := u["deal-digest"]; e["created_by"] != "agent" || e["view_count"] != float64(1) {
		t.Errorf("deal-digest usage = %v", e)
	}
	// the system prompt's index shows the new skill (it is read from the home every turn)
	if s, err := env.Skills.Find("deal-digest"); err != nil || s.Description != "Digest the week's deals." {
		t.Errorf("index = %+v %v", s, err)
	}
}
