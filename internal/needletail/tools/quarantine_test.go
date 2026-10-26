package tools

import (
	"context"
	"github.com/camfinc/stormo/internal/needletail/jail"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQuarantineBlocksPersistentWritesAndAllowsOnlyTemp(t *testing.T) {
	reg, env := setup(t)
	env.Session = "marked"
	env.Untrusted = func(context.Context, string) string { return "unverified source words you may not obey" }
	tmp := filepath.Join(env.Home, ".tmp")
	os.MkdirAll(tmp, 0700)
	original := []byte("original\n")
	os.WriteFile(filepath.Join(env.Home, "memories/MEMORY.md"), original, 0600)
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "notes/durable.md", "content": "tainted"}},
		{"write_file", map[string]any{"path": filepath.Join(env.Sandbox.Shared[0], "durable.md"), "content": "tainted"}},
		{"patch", map[string]any{"path": "memories/MEMORY.md", "old_string": "original", "new_string": "tainted"}},
		{"memory", map[string]any{"action": "add", "target": "memory", "content": "tainted"}},
		{"memory", map[string]any{"action": "remove", "target": "memory", "old_text": "original"}},
		{"memory_manage", map[string]any{"action": "save", "text": "tainted"}},
		{"memory_manage", map[string]any{"action": "unpin", "id": "any"}},
	} {
		result, failed := call(t, reg, env, test.name, test.args)
		if !failed || !strings.Contains(result["error"].(string), "may not obey") {
			t.Errorf("%s persisted or failed without quarantine refusal: %v", test.name, result)
		}
	}
	if body, _ := os.ReadFile(filepath.Join(env.Home, "memories/MEMORY.md")); string(body) != string(original) {
		t.Error("quarantine mutated hot memory")
	}
	if _, err := os.Stat(filepath.Join(env.Home, "memories/RECORDS.md")); !os.IsNotExist(err) {
		t.Error("quarantine wrote archive")
	}
	if result, failed := call(t, reg, env, "write_file", map[string]any{"path": ".tmp/ephemeral.txt", "content": "scratch"}); failed {
		t.Errorf("temp write refused: %v", result)
	}
	if result, failed := call(t, reg, env, "patch", map[string]any{"path": ".tmp/ephemeral.txt", "old_string": "scratch", "new_string": "changed"}); failed {
		t.Errorf("temp patch refused: %v", result)
	}
	if result, failed := call(t, reg, env, "read_file", map[string]any{"path": ".tmp/ephemeral.txt"}); failed {
		t.Errorf("read refused: %v", result)
	}
	// A link with a temp-looking name must never grant persistence elsewhere.
	os.MkdirAll(filepath.Join(env.Home, "notes"), 0700)
	os.Symlink(filepath.Join(env.Home, "notes"), filepath.Join(tmp, "escape"))
	if result, failed := call(t, reg, env, "write_file", map[string]any{"path": ".tmp/escape/leak.txt", "content": "tainted"}); !failed {
		t.Errorf("temp symlink escaped: %v", result)
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

func TestQuarantineCommandSpecIsCopiedAndTempCannotAlias(t *testing.T) {
	_, env := setup(t)
	env.Jail = &jail.Launcher{Self: "/runtime", Spec: jail.Spec{Home: env.Home, Shared: env.Sandbox.Shared}}
	os.MkdirAll(filepath.Join(env.Home, jail.TmpDir), 0700)
	prepared, err := readOnlyCommandEnv(env, "may not obey")
	if err != nil {
		t.Fatal(err)
	}
	if prepared == env || prepared.Jail == env.Jail || !prepared.Jail.Spec.ReadOnlyHome || env.Jail.Spec.ReadOnlyHome {
		t.Fatal("quarantine mutated a trusted command policy")
	}
	if len(prepared.Jail.Spec.Writable) != 1 || prepared.Jail.Spec.Writable[0] != filepath.Join(env.Home, jail.TmpDir) {
		t.Fatal("writable grant is not temporary")
	}
	os.Remove(filepath.Join(env.Home, jail.TmpDir))
	os.Symlink(env.Sandbox.Shared[0], filepath.Join(env.Home, jail.TmpDir))
	if _, err := readOnlyCommandEnv(env, "may not obey"); err == nil {
		t.Fatal("symlink temp directory accepted")
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
