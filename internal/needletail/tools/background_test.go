//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bgSetup is setup with a process registry and the conversation each session belongs to (a
// session "<conv>/<n>" is conversation <conv>, as a compaction's child keeps its root).
func bgSetup(t *testing.T) (*Registry, *Env, *Processes) {
	t.Helper()
	r, env := setup(t)
	p := &Processes{}
	t.Cleanup(func() { p.Shutdown(context.Background()) })
	env.Processes = p
	env.Conversation = func(session string) string { conv, _, _ := strings.Cut(session, "/"); return conv }
	env.Session = "conv-a/1"
	return r, env, p
}

// started is the process id a terminal call with background: true answered.
func started(t *testing.T, r *Registry, env *Env, command string) (string, int) {
	t.Helper()
	m, failed := call(t, r, env, "terminal", map[string]any{"command": command, "background": true})
	id, _ := m["session_id"].(string)
	pid, _ := m["pid"].(float64)
	if failed || !strings.HasPrefix(id, "proc_") || pid <= 0 {
		t.Fatalf("background start = %v", m)
	}
	return id, int(pid)
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("process %d still alive", pid)
}

// A background command returns at once with its id; poll gives the output written since the last
// poll (redacted), wait gives the exit, log pages the whole output by lines.
func TestBackgroundPollWaitLog(t *testing.T) {
	r, env, _ := bgSetup(t)
	start := time.Now()
	id, _ := started(t, r, env, `echo "first token=$CRM_API_TOKEN"; read line; echo "got $line"; for i in 1 2 3; do echo "line $i"; done; exit 4`)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("background start blocked for %s", time.Since(start))
	}
	deadline := time.Now().Add(10 * time.Second)
	var m map[string]any
	for time.Now().Before(deadline) {
		m, _ = call(t, r, env, "process_manage", map[string]any{"action": "poll", "session_id": id})
		if strings.Contains(fmt.Sprint(m["output"]), "first") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if m["status"] != "running" || !strings.Contains(m["output"].(string), "first token=[REDACTED]") || strings.Contains(m["output"].(string), "acme_abcdefgh123") {
		t.Fatalf("poll = %v", m)
	}
	// nothing new since the last poll
	if m, _ := call(t, r, env, "process_manage", map[string]any{"action": "poll", "session_id": id}); m["output"] != "" {
		t.Errorf("second poll = %v", m)
	}
	// a unique prefix of the id works
	if m, failed := call(t, r, env, "process_manage", map[string]any{"action": "submit", "session_id": id[:9], "data": "hello"}); failed || m["status"] != "ok" {
		t.Fatalf("submit = %v", m)
	}
	m, failed := call(t, r, env, "process_manage", map[string]any{"action": "wait", "session_id": id, "timeout": 10})
	if failed || m["status"] != "exited" || m["exit_code"] != float64(4) || !strings.Contains(m["output"].(string), "line 3") {
		t.Fatalf("wait = %v", m)
	}
	m, _ = call(t, r, env, "process_manage", map[string]any{"action": "poll", "session_id": id})
	if m["status"] != "exited" || m["exit_code"] != float64(4) || !strings.Contains(m["output"].(string), "got hello") || strings.Contains(m["output"].(string), "first") {
		t.Errorf("poll after exit = %v", m)
	}
	m, _ = call(t, r, env, "process_manage", map[string]any{"action": "log", "session_id": id, "offset": 0, "limit": 2})
	if m["output"] != "first token=[REDACTED]\ngot hello" || m["total_lines"] != float64(5) {
		t.Errorf("log head = %v", m)
	}
	m, _ = call(t, r, env, "process_manage", map[string]any{"action": "log", "session_id": id, "limit": 1})
	if m["output"] != "line 3" {
		t.Errorf("log tail = %v", m)
	}
	m, _ = call(t, r, env, "process_manage", map[string]any{"action": "list"})
	ps, _ := m["processes"].([]any)
	if len(ps) != 1 || ps[0].(map[string]any)["session_id"] != id || ps[0].(map[string]any)["status"] != "exited" {
		t.Errorf("list = %v", m)
	}
	if m, failed := call(t, r, env, "process_manage", map[string]any{"action": "write", "session_id": id, "data": "late"}); !failed {
		t.Errorf("write after exit = %v", m)
	}
}

// kill ends the whole process group (a child it started too); a process another conversation
// started is not found, and a compaction's child session is the same conversation.
func TestBackgroundKillAndScope(t *testing.T) {
	r, env, _ := bgSetup(t)
	marker := filepath.Join(env.Home, "child-pid")
	id, pid := started(t, r, env, `sleep 300 & echo $! > `+marker+`; echo up; wait`)
	var child int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && child == 0 {
		b, _ := os.ReadFile(marker)
		fmt.Sscan(strings.TrimSpace(string(b)), &child)
		time.Sleep(20 * time.Millisecond)
	}
	if child == 0 {
		t.Fatal("no child pid")
	}
	other := *env
	other.Session = "conv-b/1"
	for _, action := range []string{"poll", "kill", "log", "wait", "write", "close"} {
		if m, failed := call(t, r, &other, "process_manage", map[string]any{"action": action, "session_id": id, "data": "x", "timeout": 1}); !failed || m["status"] != "not_found" && !strings.Contains(fmt.Sprint(m["error"]), "no process") {
			t.Errorf("%s from another conversation = %v", action, m)
		}
	}
	if m, _ := call(t, r, &other, "process_manage", map[string]any{"action": "list"}); len(m["processes"].([]any)) != 0 {
		t.Errorf("another conversation lists %v", m)
	}
	compacted := *env
	compacted.Session = "conv-a/2"
	m, failed := call(t, r, &compacted, "process_manage", map[string]any{"action": "kill", "session_id": id})
	if failed || m["status"] != "killed" {
		t.Fatalf("kill = %v", m)
	}
	waitGone(t, pid)
	waitGone(t, child)
	if m, _ := call(t, r, env, "process_manage", map[string]any{"action": "kill", "session_id": id}); m["status"] != "already_exited" {
		t.Errorf("second kill = %v", m)
	}
}

// At most PerConversation running per conversation and PerAgent in all; a finished one frees
// its slot.
func TestBackgroundBounds(t *testing.T) {
	r, env, p := bgSetup(t)
	p.PerConversation, p.PerAgent = 2, 3
	a1, _ := started(t, r, env, "sleep 300")
	started(t, r, env, "sleep 300")
	if m, failed := call(t, r, env, "terminal", map[string]any{"command": "sleep 300", "background": true}); !failed || !strings.Contains(fmt.Sprint(m["error"]), "2 background processes") {
		t.Errorf("third in a conversation = %v", m)
	}
	b := *env
	b.Session = "conv-b/1"
	started(t, r, &b, "sleep 300")
	if m, failed := call(t, r, &b, "terminal", map[string]any{"command": "sleep 300", "background": true}); !failed || !strings.Contains(fmt.Sprint(m["error"]), "3 background processes") {
		t.Errorf("fourth for the agent = %v", m)
	}
	call(t, r, env, "process_manage", map[string]any{"action": "kill", "session_id": a1})
	started(t, r, &b, "sleep 300")
}

// The output buffer keeps its head and its tail; the middle is elided, and a secret straddling a
// read never comes out in part.
func TestBackgroundRing(t *testing.T) {
	r, env, p := bgSetup(t)
	p.Buffer = 64 << 10
	id, _ := started(t, r, env, `printf 'HEAD\n'; head -c 300000 /dev/zero | tr '\0' x; printf '\nmid acme_abcdefgh123 tail\nEND\n'`)
	m, _ := call(t, r, env, "process_manage", map[string]any{"action": "wait", "session_id": id, "timeout": 20})
	if m["status"] != "exited" {
		t.Fatalf("wait = %v", m)
	}
	m, _ = call(t, r, env, "process_manage", map[string]any{"action": "log", "session_id": id, "offset": 0, "limit": 1000})
	out := m["output"].(string)
	if !strings.HasPrefix(out, "HEAD\n") || !strings.HasSuffix(out, "mid [REDACTED] tail\nEND") || !strings.Contains(out, "bytes elided") || len(out) > 70<<10 {
		t.Errorf("log: %d bytes, head %q tail %q", len(out), out[:min(20, len(out))], out[max(0, len(out)-40):])
	}
	if strings.Contains(out, "acme_") {
		t.Error("part of a secret in the output")
	}
}

// Every write is redacted as it streams in: a secret split across two writes is masked whole.
func TestBackgroundStreamRedaction(t *testing.T) {
	var got strings.Builder
	s := &streamRedactor{redact: func(x string) string { return strings.ReplaceAll(x, "acme_abcdefgh123", "[REDACTED]") }, overlap: 32,
		emit: func(b string) { got.WriteString(b) }}
	for _, part := range []string{"a acme_abc", "defgh123 b ", strings.Repeat("y", 100), " acme_", "abcdefgh123"} {
		s.write([]byte(part))
		if strings.Contains(got.String(), "acme_") {
			t.Fatalf("emitted part of a secret: %q", got.String())
		}
	}
	s.flush()
	want := "a [REDACTED] b " + strings.Repeat("y", 100) + " [REDACTED]"
	if got.String() != want {
		t.Errorf("got %q, want %q", got.String(), want)
	}
}

// Ending a conversation (deleted, or ended for good) kills its processes and no other; the
// lifetime kills one that runs too long; shutdown kills the rest.
func TestBackgroundEnds(t *testing.T) {
	r, env, p := bgSetup(t)
	_, pa := started(t, r, env, "sleep 300")
	b := *env
	b.Session = "conv-b/1"
	_, pb := started(t, r, &b, "sleep 300")
	p.EndConversation("conv-a", "deleted")
	waitGone(t, pa)
	if !alive(pb) {
		t.Fatal("another conversation's process was killed")
	}
	p.Lifetime = 300 * time.Millisecond
	c := *env
	c.Session = "conv-c/1"
	id, pc := started(t, r, &c, "sleep 300")
	waitGone(t, pc)
	m, _ := call(t, r, &c, "process_manage", map[string]any{"action": "poll", "session_id": id})
	if m["status"] != "exited" || m["termination_source"] != "lifetime" {
		t.Errorf("after the lifetime: %v", m)
	}
	p.Shutdown(context.Background())
	waitGone(t, pb)
	if m, failed := call(t, r, &b, "terminal", map[string]any{"command": "sleep 1", "background": true}); !failed {
		t.Errorf("started after shutdown: %v", m)
	}
}

// Without a registry (a Luau-only runtime, a test) background is refused, never run in the
// foreground; process_manage says so.
func TestBackgroundUnavailable(t *testing.T) {
	r, env := setup(t)
	if m, failed := call(t, r, env, "terminal", map[string]any{"command": "echo hi", "background": true}); !failed || !strings.Contains(fmt.Sprint(m["error"]), "not available") {
		t.Errorf("background without a registry = %v", m)
	}
	if m, failed := call(t, r, env, "process_manage", map[string]any{"action": "list"}); !failed {
		t.Errorf("process_manage without a registry = %v", m)
	}
}

// process_manage's hooks carry metadata only: never what is written to stdin.
func TestProcessManageHookArgs(t *testing.T) {
	r, _ := setup(t)
	tool, _ := r.Get("process_manage")
	got := tool.HookArgs(`{"action":"write","session_id":"proc_0123456789ab","data":"my password is hunter2","timeout":5}`)
	var m map[string]any
	_ = json.Unmarshal([]byte(got), &m)
	if m["action"] != "write" || m["session_id"] != "proc_0123456789ab" || m["has_data"] != true || m["timeout"] != float64(5) || strings.Contains(got, "hunter2") {
		t.Errorf("hook args = %s", got)
	}
}

// A write to a command that does not read its stdin gives up after stdinWait instead of holding
// the turn; the process is still there to poll or kill.
func TestBackgroundStdinDeadline(t *testing.T) {
	r, env, _ := bgSetup(t)
	stdinWait = 300 * time.Millisecond
	t.Cleanup(func() { stdinWait = 5 * time.Second })
	id, _ := started(t, r, env, "sleep 300")
	start := time.Now()
	var m map[string]any
	failed := false
	for i := 0; i < 8 && !failed; i++ { // the pipe's buffer takes the first ones (64 KiB or more)
		m, failed = call(t, r, env, "process_manage", map[string]any{"action": "write", "session_id": id, "data": strings.Repeat("x", maxStdin)})
	}
	if !failed || !strings.Contains(fmt.Sprint(m["error"]), "not reading its stdin") || time.Since(start) > 5*time.Second {
		t.Errorf("write to a command that does not read = %v after %s", m, time.Since(start))
	}
	if m, _ := call(t, r, env, "process_manage", map[string]any{"action": "poll", "session_id": id}); m["status"] != "running" {
		t.Errorf("after the refused write: %v", m)
	}
}
