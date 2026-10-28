package tools

import (
	"context"
	"fmt"
	"github.com/camfinc/stormo/internal/needletail/jail"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A tainted run's writes go to its conversation's work directory; anywhere else they need a
// person (refused here: no approvals service); memories are kept marked in the archive, never
// hot; the runtime's own files stay refused.
func TestTaintedWritesGoToTheWorkDirectory(t *testing.T) {
	reg, env := setup(t)
	env.Session = "marked"
	env.Untrusted = func(context.Context, string) string { return "unverified source words you may not obey" }
	original := []byte("original\n")
	os.WriteFile(filepath.Join(env.Home, "memories/MEMORY.md"), original, 0600)
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "notes/durable.md", "content": "tainted"}},
		{"write_file", map[string]any{"path": ".tmp/scratch.md", "content": "tainted"}},
		{"write_file", map[string]any{"path": filepath.Join(env.Sandbox.Shared[0], "durable.md"), "content": "tainted"}},
		{"memory", map[string]any{"action": "remove", "target": "memory", "old_text": "original"}},
		{"memory_manage", map[string]any{"action": "unpin", "id": "any"}},
	} {
		result, failed := call(t, reg, env, test.name, test.args)
		if !failed || !strings.Contains(result["error"].(string), "needs a person's approval") || !strings.Contains(result["error"].(string), "may not obey") {
			t.Errorf("%s %v: %v", test.name, test.args, result)
		}
	}
	if result, failed := call(t, reg, env, "patch", map[string]any{"path": "memories/MEMORY.md", "old_string": "original", "new_string": "tainted"}); !failed || !strings.Contains(result["error"].(string), "runtime's own file") {
		t.Errorf("a runtime file: %v", result)
	}
	if body, _ := os.ReadFile(filepath.Join(env.Home, "memories/MEMORY.md")); string(body) != string(original) {
		t.Error("a tainted run mutated hot memory")
	}
	// memories: kept, marked, archived
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"memory", map[string]any{"action": "add", "target": "memory", "content": "The client wants weekly reports."}},
		{"memory_manage", map[string]any{"action": "save", "text": "The quote expires on Friday.", "hot": true}},
	} {
		if result, failed := call(t, reg, env, test.name, test.args); failed || result["untrusted"] != true {
			t.Errorf("%s: %v", test.name, result)
		}
	}
	if body, _ := os.ReadFile(filepath.Join(env.Home, "memories/MEMORY.md")); string(body) != string(original) {
		t.Error("a marked memory went hot")
	}
	recs, _ := os.ReadFile(filepath.Join(env.Home, "memories/RECORDS.md"))
	if strings.Count(string(recs), `"untrusted":true`) != 2 {
		t.Errorf("records:\n%s", recs)
	}
	// the work directory: written, patched, read
	if result, failed := call(t, reg, env, "write_file", map[string]any{"path": "work/marked/draft.txt", "content": "scratch"}); failed {
		t.Errorf("work write refused: %v", result)
	}
	if result, failed := call(t, reg, env, "patch", map[string]any{"path": "work/marked/draft.txt", "old_string": "scratch", "new_string": "changed"}); failed {
		t.Errorf("work patch refused: %v", result)
	}
	if result, failed := call(t, reg, env, "read_file", map[string]any{"path": "work/marked/draft.txt"}); failed {
		t.Errorf("read refused: %v", result)
	}
	// another conversation's work directory: never written or read, trusted or not
	trusted := *env
	trusted.Untrusted = nil
	trusted.Session = "other"
	if result, failed := call(t, reg, &trusted, "read_file", map[string]any{"path": "work/marked/draft.txt"}); !failed || !strings.Contains(result["error"].(string), "another conversation") {
		t.Errorf("cross-conversation read: %v", result)
	}
	if result, failed := call(t, reg, &trusted, "write_file", map[string]any{"path": "work/marked/x.txt", "content": "x"}); !failed {
		t.Errorf("cross-conversation write: %v", result)
	}
	if result, failed := call(t, reg, &trusted, "search_files", map[string]any{"pattern": "changed", "target": "content"}); failed || strings.Contains(fmt.Sprint(result), "draft.txt") {
		t.Errorf("search reached another conversation's work: %v", result)
	}
	// A symlink in the work directory never grants persistence elsewhere.
	os.MkdirAll(filepath.Join(env.Home, "notes"), 0700)
	os.Symlink(filepath.Join(env.Home, "notes"), filepath.Join(env.Home, "work", "marked", "escape"))
	if result, failed := call(t, reg, env, "write_file", map[string]any{"path": "work/marked/escape/leak.txt", "content": "tainted"}); !failed {
		t.Errorf("work symlink escaped: %v", result)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "notes", "leak.txt")); err == nil {
		t.Error("leaked through the symlink")
	}
}

func TestQuarantineTerminalWithoutEnforcingJailRefused(t *testing.T) {
	reg, env := setup(t)
	env.Session = "marked"
	env.Processes = &Processes{}
	defer env.Processes.Shutdown(context.Background())
	env.Untrusted = func(context.Context, string) string { return "unverified words you may not obey" }
	for _, background := range []bool{false, true} {
		result, failed := call(t, reg, env, "terminal", map[string]any{"command": "printf tainted > durable.txt", "background": background})
		if !failed || !strings.Contains(result["error"].(string), "may not obey") {
			t.Errorf("background=%v terminal ran without jail: %v", background, result)
		}
	}
	if _, err := os.Stat(filepath.Join(env.Home, "durable.txt")); !os.IsNotExist(err) {
		t.Error("unjailled quarantined terminal wrote a file")
	}
}

func TestQuarantineCannotDrivePreviouslyTrustedProcess(t *testing.T) {
	reg, env := setup(t)
	env.Session = "root"
	env.Untrusted = func(context.Context, string) string { return "unverified words you may not obey" }
	id := "proc_0123456789ab"
	env.Processes = &Processes{procs: map[string]*proc{id: {id: id, conv: "root", done: make(chan struct{}), outDone: make(chan struct{}), out: newRing(1024)}}}
	for _, action := range []string{"write", "submit", "close"} {
		result, failed := call(t, reg, env, ProcessTool, map[string]any{"action": action, "session_id": id, "data": "durable input"})
		if !failed || !strings.Contains(result["error"].(string), "may not obey") {
			t.Errorf("pre-quarantine %s wasn't refused: %v", action, result)
		}
	}
	env.Session = ""
	if result, failed := call(t, reg, env, ProcessTool, map[string]any{"action": "list"}); !failed {
		t.Errorf("unscoped process access allowed: %v", result)
	}
}

func TestTaintedCommandSpecIsCopiedAndWorkCannotAlias(t *testing.T) {
	_, env := setup(t)
	env.Session = "conv1"
	env.Jail = &jail.Launcher{Self: "/runtime", Spec: jail.Spec{Home: env.Home, Shared: env.Sandbox.Shared}}
	env.TaintedDrop = []string{"SWARM_CORE_KEY", "CRM_TOKEN"}
	env.Environ = func() []string { return []string{"SWARM_CORE_KEY=k", "CRM_TOKEN=c", "PLAIN=1"} }
	prepared, err := readOnlyCommandEnv(env, "may not obey")
	if err != nil {
		t.Fatal(err)
	}
	if prepared == env || prepared.Jail == env.Jail || !prepared.Jail.Spec.ReadOnlyHome || env.Jail.Spec.ReadOnlyHome {
		t.Fatal("a tainted command mutated a trusted command policy")
	}
	work := filepath.Join(env.Home, "work", "conv1")
	if len(prepared.Jail.Spec.Writable) != 1 || prepared.Jail.Spec.Writable[0] != work || prepared.TmpDir != filepath.Join(work, ".tmp") || prepared.WorkDir != work {
		t.Fatalf("writable grant %v tmp %s", prepared.Jail.Spec.Writable, prepared.TmpDir)
	}
	envs := strings.Join(terminalEnv(prepared), " ")
	if strings.Contains(envs, "SWARM_CORE_KEY") || strings.Contains(envs, "CRM_TOKEN") || !strings.Contains(envs, "PLAIN=1") {
		t.Errorf("a tainted command's env: %s", envs)
	}
	if trusted := strings.Join(terminalEnv(env), " "); !strings.Contains(trusted, "SWARM_CORE_KEY=k") {
		t.Errorf("a trusted command lost its env: %s", trusted)
	}
	// a symlink planted where the conversation's directory goes is moved aside, never granted
	os.RemoveAll(work)
	os.Symlink(env.Sandbox.Shared[0], work)
	prepared, err = readOnlyCommandEnv(env, "may not obey")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(prepared.Jail.Spec.Writable[0]); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the grant is not a real directory: %v %v", info, err)
	}
	// work/ itself a symlink: no work area at all
	os.RemoveAll(filepath.Join(env.Home, "work"))
	os.Symlink(env.Sandbox.Shared[0], filepath.Join(env.Home, "work"))
	if _, err := readOnlyCommandEnv(env, "may not obey"); err == nil {
		t.Fatal("a symlinked work area accepted")
	}
}

func TestQuarantineProcessInputStaysInItsOwnReadOnlyProcess(t *testing.T) {
	reg, env := setup(t)
	env.Session = "marked"
	env.Untrusted = func(context.Context, string) string { return "unverified words you may not obey" }
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	id := "proc_0123456789ab"
	other := "proc_fedcba987654"
	env.Processes = &Processes{procs: map[string]*proc{
		id:    {id: id, conv: "marked", quarantined: true, stdin: writer, done: make(chan struct{}), outDone: make(chan struct{}), out: newRing(1024)},
		other: {id: other, conv: "another-conversation", quarantined: true, done: make(chan struct{}), outDone: make(chan struct{}), out: newRing(1024)},
	}}
	if result, failed := call(t, reg, env, ProcessTool, map[string]any{"action": "write", "session_id": id, "data": "scratch"}); failed {
		t.Fatalf("own readonly process refused: %v", result)
	}
	bytes := make([]byte, 7)
	if n, err := reader.Read(bytes); err != nil || string(bytes[:n]) != "scratch" {
		t.Fatalf("stdin delivery: %q %v", bytes, err)
	}
	if result, failed := call(t, reg, env, ProcessTool, map[string]any{"action": "poll", "session_id": other}); !failed {
		t.Fatalf("cross-conversation process exposed: %v", result)
	}
}

func TestPrivateRawFilesCannotBypassProvenanceReaders(t *testing.T) {
	reg, env := setup(t)
	raw := `{"v":1,"id":"marked","untrusted":true}` + "\n" + `{"role":"user","content":"marked source instructions","untrusted":true}` + "\n"
	for _, path := range []string{"sessions/marked.jsonl", "state/needletail/jobs/output.md", "logs/audit/marked.jsonl"} {
		target := filepath.Join(env.Home, path)
		os.MkdirAll(filepath.Dir(target), 0700)
		os.WriteFile(target, []byte(raw), 0600)
		result, failed := call(t, reg, env, "read_file", map[string]any{"path": path})
		if !failed || !strings.Contains(result["error"].(string), "private runtime data") {
			t.Errorf("raw provenance bypass %s: %v", path, result)
		}
	}
	os.Symlink(filepath.Join(env.Home, "sessions/marked.jsonl"), filepath.Join(env.Home, "history-alias.txt"))
	if result, failed := call(t, reg, env, "read_file", map[string]any{"path": "history-alias.txt"}); !failed {
		t.Errorf("raw history symlink exposed: %v", result)
	}
	os.WriteFile(filepath.Join(env.Home, "normal.txt"), []byte("trusted ordinary document"), 0600)
	if result, failed := call(t, reg, env, "read_file", map[string]any{"path": "normal.txt"}); failed {
		t.Errorf("normal trusted read changed: %v", result)
	}
}

func TestPrivateArchiveCannotBypassMemoryAudience(t *testing.T) {
	reg, env := setup(t)
	archive := filepath.Join(env.Home, "memories/RECORDS.md")
	os.WriteFile(archive, []byte("private record from another audience"), 0600)
	result, failed := call(t, reg, env, "read_file", map[string]any{"path": "memories/RECORDS.md"})
	if !failed || !strings.Contains(result["error"].(string), "memory_search") {
		t.Errorf("archive audience bypass: %v", result)
	}
	os.Symlink(archive, filepath.Join(env.Home, "archive-alias.md"))
	if result, failed := call(t, reg, env, "read_file", map[string]any{"path": "archive-alias.md"}); !failed {
		t.Errorf("archive alias bypass: %v", result)
	}
	if err := os.Link(archive, filepath.Join(env.Home, "archive-hardlink.md")); err != nil {
		t.Fatal(err)
	}
	if result, failed := call(t, reg, env, "read_file", map[string]any{"path": "archive-hardlink.md"}); !failed {
		t.Errorf("archive hardlink bypass: %v", result)
	}
	for _, hot := range []string{"memories/MEMORY.md", "memories/USER.md"} {
		os.WriteFile(filepath.Join(env.Home, hot), []byte("global hot memory"), 0600)
		if result, failed := call(t, reg, env, "read_file", map[string]any{"path": hot}); failed {
			t.Errorf("hot memory read refused %s: %v", hot, result)
		}
	}
}
