package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/skills"
)

// Background processes (docs/needletail.md §4.5): `terminal` with background: true starts a
// command the way the terminal starts any (bash with no startup file, the agent's user, the
// jail, the scrubbed environment and TMPDIR) and returns its id at once; process_manage polls,
// waits on, reads, writes to and kills it. The registry is the runtime's, in memory only: no file
// in the home, nothing napped. A process belongs to the conversation (its root session) whose
// turn started it: another conversation's are invisible. They are killed when their
// conversation is deleted or ends for good (a scheduled run's, a oneshot's), at their lifetime,
// and when the runtime shuts down; a compaction keeps them (the conversation goes on).
//
// Unlike `cmd &` inside a normal command (untracked: its later output is lost, nothing kills
// it, nothing counts it), a background process is tracked, its output (redacted as it arrives)
// kept in a bounded buffer, and it is bounded in number and lifetime.

// Defaults of a zero Processes.
const (
	DefaultPerConversation = 4
	DefaultPerAgent        = 8
	DefaultBuffer          = 1 << 20
	DefaultLifetime        = 2 * time.Hour
	// MaxLifetime is the longest a config may set (needletail.yaml limits.background_lifetime).
	MaxLifetime = 24 * time.Hour
	// keepFinished are the finished processes kept for poll and log, per agent, the oldest
	// pruned first, and none longer than keepFinishedFor.
	keepFinished    = 16
	keepFinishedFor = time.Hour
	// killGrace is how long a process group gets after SIGTERM before SIGKILL.
	killGrace = 2 * time.Second
	// result bounds: a poll's new output, a log page, an exit snapshot, stdin per write
	pollBytes = 16 << 10
	logBytes  = 50 << 10
	tailBytes = 4 << 10
	maxStdin  = 64 << 10
	// idleFlush is how long the output may be quiet before what the redactor holds back (the
	// end of the output, in case a secret goes on) is let out: a process that writes part of a
	// secret and pauses this long shows that part.
	idleFlush = 200 * time.Millisecond
)

// ProcessTool is process_manage's name (Hermes').
const ProcessTool = "process_manage"

// Processes is the agent's background processes. The zero value uses the defaults; it is safe
// for concurrent use.
type Processes struct {
	// PerConversation and PerAgent bound the processes running at once.
	PerConversation, PerAgent int
	// Buffer is each process' output buffer (bytes): its first quarter and its last
	// three-quarters are kept, the middle elided.
	Buffer int
	// Lifetime is how long a process may run before it is killed.
	Lifetime time.Duration
	Log      func(string)

	mu     sync.Mutex
	procs  map[string]*proc
	closed bool
}

func (p *Processes) logf(format string, args ...any) {
	if p.Log != nil {
		p.Log(fmt.Sprintf(format, args...))
	}
}

func (p *Processes) perConversation() int {
	return orDefault(p.PerConversation, DefaultPerConversation)
}
func (p *Processes) perAgent() int { return orDefault(p.PerAgent, DefaultPerAgent) }

func (p *Processes) lifetime() time.Duration {
	if p.Lifetime <= 0 {
		return DefaultLifetime
	}
	return min(p.Lifetime, MaxLifetime)
}

func orDefault(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

// proc is one background process.
type proc struct {
	id, conv, command string
	quarantined       bool
	pid               int
	started           time.Time
	cancel            context.CancelFunc // kills the group (cmd.Cancel)
	out               *ring
	done              chan struct{} // closed when the direct child has exited
	outDone           chan struct{} // closed when the output is closed (every holder exited)
	lifetime          *time.Timer

	// writeMu serialises writes to stdin; mu is never held across one
	writeMu  sync.Mutex
	mu       sync.Mutex
	stdin    *os.File // nil once closed
	exitCode int
	signal   string
	ended    time.Time
	source   string // what ended it: exited, killed, lifetime, shutdown, deleted, ended
	polled   int64  // the stream offset the last poll read to
}

func (pr *proc) exited() bool {
	select {
	case <-pr.done:
		return true
	default:
		return false
	}
}

// ---- starting ---------------------------------------------------------------------------------

// errNoProcesses: the runtime gave the terminal no registry.
var errNoProcesses = errors.New("background processes are not available in this runtime")

// startBackground is the terminal's background: true.
func startBackground(env *Env, a terminalArgs, dir, skillDir string) (any, error) {
	p := env.Processes
	if p == nil {
		return nil, errNoProcesses
	}
	conv := conversationOf(env)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("the runtime is shutting down: no process starts")
	}
	p.prune()
	inConv, all := 0, 0
	for _, pr := range p.procs {
		if !pr.exited() {
			all++
			if pr.conv == conv {
				inConv++
			}
		}
	}
	if inConv >= p.perConversation() {
		p.mu.Unlock()
		return nil, fmt.Errorf("this conversation already has %d background processes running (the most): kill one (process_manage kill) or wait for one to end", inConv)
	}
	if all >= p.perAgent() {
		p.mu.Unlock()
		return nil, fmt.Errorf("you already have %d background processes running in all (the most): kill one or wait for one to end", all)
	}
	pr, err := p.spawn(env, a.Command, conv, dir, skillDir)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	if p.procs == nil {
		p.procs = map[string]*proc{}
	}
	p.procs[pr.id] = pr
	p.mu.Unlock()
	p.logf("background process %s started (pid %d, conversation %s)", pr.id, pr.pid, conv)
	return map[string]any{"output": "Background process started", "session_id": pr.id, "pid": pr.pid, "exit_code": 0,
		"note": fmt.Sprintf("It runs until it exits, is killed, or %s pass; you are not notified when it ends: "+
			"process_manage poll (new output), wait, log, write, kill with session_id %s.", p.lifetime(), pr.id)}, nil
}

// conversationOf is the calling turn's conversation (its root session; the session itself when
// the runtime cannot say).
func conversationOf(env *Env) string {
	if env.Conversation != nil {
		if c := env.Conversation(env.Session); c != "" {
			return c
		}
	}
	return env.Session
}

// spawn starts the command; p.mu is held.
func (p *Processes) spawn(env *Env, command, conv, dir, skillDir string) (*proc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-c", command)
	cmd.Dir = dir
	cmd.Env = terminalEnv(env)
	if skillDir != "" {
		cmd.Env = append(cmd.Env, skills.DirVar+"="+skillDir)
	}
	// stdin and the output are pipes the runtime holds the other end of (pollable, so a write
	// to a command that does not read has a deadline)
	inR, inW, err := os.Pipe()
	if err != nil {
		cancel()
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		cancel()
		_, _ = inR.Close(), inW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, w, w
	Command(cmd, env) // its own process group, the agent's user, the jail
	if err := cmd.Start(); err != nil {
		cancel()
		_, _, _, _ = r.Close(), w.Close(), inR.Close(), inW.Close()
		return nil, fmt.Errorf("could not start the command: %v", err)
	}
	_, _ = w.Close(), inR.Close() // the command's copies hold them now
	buf := p.Buffer
	if buf <= 0 {
		buf = DefaultBuffer
	}
	pr := &proc{id: newProcID(), conv: conv, command: command, quarantined: env.Jail != nil && env.Jail.Spec.ReadOnlyHome, pid: cmd.Process.Pid, started: time.Now(), cancel: cancel,
		out: newRing(buf), done: make(chan struct{}), outDone: make(chan struct{}), stdin: inW}
	pr.lifetime = time.AfterFunc(p.lifetime(), func() { p.kill(pr, "lifetime") })
	overlap := 0
	if env.LongestSecret != nil {
		overlap = 2 * env.LongestSecret() // a mask can be longer than what it replaces
	}
	sr := &streamRedactor{redact: env.Redact, overlap: overlap, emit: pr.out.write}
	go func() { // the output, until every process holding it has exited
		defer close(pr.outDone)
		defer r.Close()
		b := make([]byte, 32<<10)
		for {
			// what is held back for a secret that may go on is let out once the process has
			// been quiet a moment, so a prompt waiting for input is seen
			if sr.pending != "" {
				_ = r.SetReadDeadline(time.Now().Add(idleFlush))
			} else {
				_ = r.SetReadDeadline(time.Time{})
			}
			n, err := r.Read(b)
			if n > 0 {
				sr.write(b[:n])
			}
			if errors.Is(err, os.ErrDeadlineExceeded) {
				sr.flush()
				continue
			}
			if err != nil {
				break
			}
		}
		sr.flush()
	}()
	go func() { // the direct child's exit
		err := cmd.Wait()
		pr.mu.Lock()
		pr.exitCode, pr.ended = cmd.ProcessState.ExitCode(), time.Now()
		if pr.source == "" {
			pr.source = "exited"
		}
		if err != nil && pr.exitCode == -1 {
			pr.signal = signalOf(cmd.ProcessState)
		}
		pr.mu.Unlock()
		pr.closeIn()
		pr.lifetime.Stop()
		close(pr.done)
		cancel()
	}()
	return pr, nil
}

func newProcID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "proc_" + hex.EncodeToString(b)
}

// ---- ending -----------------------------------------------------------------------------------

// kill ends pr's whole process group: SIGTERM, then SIGKILL after killGrace. source says why
// (the first reason given wins). It returns once the direct child has exited or the kill gave up.
func (p *Processes) kill(pr *proc, source string) {
	pr.mu.Lock()
	if pr.source == "" {
		pr.source = source
	}
	pr.mu.Unlock()
	if pr.exited() {
		select {
		case <-pr.outDone: // the whole group is gone: its id may be another's now
		default:
			_ = killGroup(pr.pid, true) // what the command left behind still holds the output
		}
		return
	}
	_ = killGroup(pr.pid, false)
	t := time.NewTimer(killGrace)
	defer t.Stop()
	select {
	case <-pr.done:
	case <-t.C:
	}
	pr.cancel() // SIGKILL to the group (cmd.Cancel), while the direct child runs
	select {
	case <-pr.outDone: // the whole group is gone
	default:
		_ = killGroup(pr.pid, true)
	}
	t2 := time.NewTimer(killGrace)
	defer t2.Stop()
	select {
	case <-pr.done:
	case <-t2.C:
		p.logf("background process %s (pid %d) did not exit after SIGKILL", pr.id, pr.pid)
	}
}

// EndConversation kills the processes of the conversation whose root is conv: it was deleted
// or ended for good (reason: deleted, ended). They are forgotten.
func (p *Processes) EndConversation(conv, reason string) {
	p.mu.Lock()
	var mine []*proc
	for id, pr := range p.procs {
		if pr.conv == conv {
			mine = append(mine, pr)
			delete(p.procs, id)
		}
	}
	p.mu.Unlock()
	killAll(p, mine, reason)
	if len(mine) > 0 {
		p.logf("conversation %s %s: %d background processes killed", conv, reason, len(mine))
	}
}

// Shutdown kills every process and refuses new ones (the runtime is exiting).
func (p *Processes) Shutdown(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	all := make([]*proc, 0, len(p.procs))
	for _, pr := range p.procs {
		all = append(all, pr)
	}
	p.mu.Unlock()
	done := make(chan struct{})
	go func() { killAll(p, all, "shutdown"); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func killAll(p *Processes, prs []*proc, reason string) {
	var wg sync.WaitGroup
	for _, pr := range prs {
		wg.Add(1)
		go func() { defer wg.Done(); p.kill(pr, reason) }()
	}
	wg.Wait()
}

// prune forgets finished processes past keepFinishedFor and, beyond keepFinished, the oldest
// finished; p.mu is held.
func (p *Processes) prune() {
	var finished []*proc
	for id, pr := range p.procs {
		if !pr.exited() {
			continue
		}
		pr.mu.Lock()
		old := time.Since(pr.ended) > keepFinishedFor
		pr.mu.Unlock()
		if old {
			delete(p.procs, id)
			continue
		}
		finished = append(finished, pr)
	}
	if len(finished) <= keepFinished {
		return
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].started.Before(finished[j].started) })
	for _, pr := range finished[:len(finished)-keepFinished] {
		delete(p.procs, pr.id)
	}
}

// ---- the tool ---------------------------------------------------------------------------------

func processTool() Tool {
	return Tool{
		Name: ProcessTool,
		Description: "Manage background processes started with terminal(background=true) in this conversation. " +
			"list: them all. poll: status and the output since the last poll. log: the output by lines (offset 0 is the start; " +
			"default the last 200). wait: until it exits or timeout seconds (at most the command timeout). " +
			"write: send data to its stdin; submit: data plus Enter; close: end its stdin. kill: stop it and what it started. " +
			"You are not notified when one ends: poll or wait.",
		Schema: schema(`{"type":"object","properties":{
"action":{"type":"string","enum":["list","poll","log","wait","kill","write","submit","close"]},
"session_id":{"type":"string","description":"The process id terminal answered (proc_…; a unique prefix works). Required except for list."},
"data":{"type":"string","description":"write, submit: the text for its stdin."},
"timeout":{"type":"integer","minimum":1,"description":"wait: seconds (default and most: the command timeout)."},
"offset":{"type":"integer","minimum":0,"description":"log: the first line (0 is the start; default: the last limit lines)."},
"limit":{"type":"integer","minimum":1,"description":"log: lines (default 200, at most 2000)."}},
"required":["action"]}`),
		Run:      runProcess,
		HookArgs: processHookArgs,
	}
}

// processHookArgs is a process_manage call as observers see it: the action, the process id's
// form, the numbers and whether data was sent, never the data (what a command reads is client
// data).
func processHookArgs(args string) string {
	base := metadata(map[string][]string{"action": {"list", "poll", "log", "wait", "kill", "write", "submit", "close"}}, nil,
		[]string{"timeout", "offset", "limit"}, nil, []string{"data"})(args)
	var in, out map[string]any
	if json.Unmarshal([]byte(args), &in) != nil || json.Unmarshal([]byte(base), &out) != nil || out == nil {
		return base
	}
	if v, ok := in["session_id"].(string); ok && procIDRe.MatchString(strings.TrimSpace(v)) {
		out["session_id"] = strings.TrimSpace(v)
	}
	b, _ := json.Marshal(out)
	return string(b)
}

type processArgs struct {
	Action    string  `json:"action"`
	SessionID string  `json:"session_id"`
	Data      *string `json:"data"`
	Timeout   int     `json:"timeout"`
	Offset    *int    `json:"offset"`
	Limit     int     `json:"limit"`
}

func runProcess(ctx context.Context, env *Env, raw json.RawMessage) (any, error) {
	var a processArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		// models send the id as a number at times
		var loose map[string]any
		if json.Unmarshal(raw, &loose) != nil {
			return nil, fmt.Errorf("bad arguments: %v", err)
		}
		if v, ok := loose["session_id"].(float64); ok {
			loose["session_id"] = fmt.Sprint(int64(v))
		}
		b, _ := json.Marshal(loose)
		if err := json.Unmarshal(b, &a); err != nil {
			return nil, fmt.Errorf("bad arguments: %v", err)
		}
	}
	p := env.Processes
	if p == nil {
		return nil, errNoProcesses
	}
	conv := conversationOf(env)
	why := UntrustedWhy(ctx, env)
	if why != "" && conv == "" {
		return nil, messaging.Deferred("process access without a known conversation", why)
	}
	if a.Action == "list" {
		return map[string]any{"processes": p.list(conv)}, nil
	}
	if strings.TrimSpace(a.SessionID) == "" {
		return nil, fmt.Errorf("session_id is required for %s", a.Action)
	}
	pr, err := p.find(conv, a.SessionID)
	if err != nil {
		return nil, err
	}
	if why != "" && !pr.quarantined && (a.Action == "write" || a.Action == "submit" || a.Action == "close") {
		return nil, messaging.Deferred("input to a process started before quarantine", why)
	}
	if why != "" && !pr.quarantined && a.Action == "kill" && !pr.exited() {
		// a process started before the conversation was marked is trusted work: killing it is a
		// disruption a person approves
		return nil, messaging.NeedsApproval("a kill of the process "+pr.id+" (started before this conversation took in untrusted input)", why)
	}
	switch a.Action {
	case "poll":
		return pr.poll(), nil
	case "log":
		return pr.log(a.Offset, a.Limit), nil
	case "wait":
		return pr.wait(ctx, env, a.Timeout), nil
	case "kill":
		if pr.exited() {
			p.kill(pr, "killed") // what it left behind, if anything
			return pr.snapshot("already_exited"), nil
		}
		p.kill(pr, "killed")
		return pr.snapshot("killed"), nil
	case "write", "submit":
		data := ""
		if a.Data != nil {
			data = *a.Data
		}
		if a.Action == "submit" {
			data += "\n"
		}
		return pr.write(data)
	case "close":
		return pr.closeStdin()
	}
	return nil, fmt.Errorf("unknown action %q: list, poll, log, wait, kill, write, submit or close", a.Action)
}

// procIDRe is a process id's form (or a prefix of one), the only id a hook names.
var procIDRe = regexp.MustCompile(`^proc_[0-9a-f]{1,12}$`)

// find is conv's process id names: the whole id or a unique prefix of it.
func (p *Processes) find(conv, id string) (*proc, error) {
	id = strings.TrimSpace(id)
	p.mu.Lock()
	defer p.mu.Unlock()
	if pr, ok := p.procs[id]; ok && pr.conv == conv {
		return pr, nil
	}
	var match *proc
	for full, pr := range p.procs {
		if pr.conv != conv || len(id) < 4 || !strings.HasPrefix(full, id) && !strings.HasPrefix(strings.TrimPrefix(full, "proc_"), id) {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("%q names more than one process: give more of the id", id)
		}
		match = pr
	}
	if match == nil {
		return nil, fmt.Errorf("no process %q in this conversation (process_manage list shows them)", id)
	}
	return match, nil
}

func (p *Processes) list(conv string) []map[string]any {
	p.mu.Lock()
	p.prune()
	var mine []*proc
	for _, pr := range p.procs {
		if pr.conv == conv {
			mine = append(mine, pr)
		}
	}
	p.mu.Unlock()
	sort.Slice(mine, func(i, j int) bool { return mine[i].started.Before(mine[j].started) })
	out := []map[string]any{}
	for _, pr := range mine {
		m := pr.head()
		m["started"] = pr.started.UTC().Format(time.RFC3339)
		m["output_bytes"] = pr.out.total()
		out = append(out, m)
	}
	return out
}

// head is the fields every answer about pr starts with.
func (pr *proc) head() map[string]any {
	m := map[string]any{"session_id": pr.id, "command": clipCommand(pr.command), "pid": pr.pid}
	if pr.exited() {
		m["status"] = "exited"
		pr.exitFields(m)
	} else {
		m["status"] = "running"
		m["uptime_seconds"] = int(time.Since(pr.started).Seconds())
	}
	return m
}

func (pr *proc) exitFields(m map[string]any) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	m["exit_code"] = pr.exitCode
	m["termination_source"] = pr.source
	if pr.signal != "" {
		m["signal"] = pr.signal
	}
	m["ran_seconds"] = int(pr.ended.Sub(pr.started).Seconds())
}

func clipCommand(c string) string {
	if len(c) <= 200 {
		return c
	}
	return strings.ToValidUTF8(c[:200], "") + "…"
}

func (pr *proc) poll() map[string]any {
	m := pr.head()
	pr.mu.Lock()
	from := pr.polled
	pr.mu.Unlock()
	text, to, skipped := pr.out.since(from, pollBytes)
	pr.mu.Lock()
	pr.polled = to
	pr.mu.Unlock()
	m["output"] = text
	if skipped > 0 {
		m["output_skipped"] = skipped
		m["note"] = "more output than a poll shows came since the last one: log reads all that is kept"
	}
	return m
}

func (pr *proc) log(offset *int, limit int) map[string]any {
	if limit <= 0 {
		limit = 200
	}
	limit = min(limit, 2000)
	lines := strings.Split(strings.TrimSuffix(pr.out.text(), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	var sel []string
	start := 0
	if offset == nil {
		start = max(0, len(lines)-limit)
	} else {
		start = min(max(0, *offset), len(lines))
	}
	sel = lines[start:min(len(lines), start+limit)]
	m := pr.head()
	m["output"] = fitOutput(strings.Join(sel, "\n"), logBytes)
	m["total_lines"] = len(lines)
	m["offset"] = start
	m["showing"] = fmt.Sprintf("%d lines", len(sel))
	return m
}

// wait blocks until pr exits, the timeout passes (at most the command timeout) or the turn is
// cancelled.
func (pr *proc) wait(ctx context.Context, env *Env, seconds int) map[string]any {
	most := env.CommandTimeout
	if most <= 0 {
		most = 180 * time.Second
	}
	most = min(most, maxTimeout)
	d := most
	note := ""
	if seconds > 0 {
		d = time.Duration(seconds) * time.Second
		if d > most {
			d, note = most, fmt.Sprintf("the wait was cut to %s, the command timeout", most)
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	var m map[string]any
	select {
	case <-pr.done:
		select { // a moment for the last of the output
		case <-pr.outDone:
		case <-time.After(200 * time.Millisecond):
		}
		m = pr.snapshot("exited")
	case <-t.C:
		m = pr.snapshot("timeout")
		m["note"] = fmt.Sprintf("still running after %s; this is not an error: poll or wait again later", d)
	case <-ctx.Done():
		m = pr.snapshot("cancelled")
	}
	if note != "" {
		m["timeout_note"] = note
	}
	return m
}

// snapshot is pr's head with status and the end of its output.
func (pr *proc) snapshot(status string) map[string]any {
	m := pr.head()
	m["status"] = status
	text, _, _ := pr.out.since(0, tailBytes)
	m["output"] = text
	return m
}

// stdinWait is how long a write may wait for a command that does not read its stdin (a
// variable for the tests).
var stdinWait = 5 * time.Second

func (pr *proc) write(data string) (any, error) {
	if len(data) > maxStdin {
		return nil, fmt.Errorf("data is %d bytes: at most %d per write", len(data), maxStdin)
	}
	pr.writeMu.Lock()
	defer pr.writeMu.Unlock()
	pr.mu.Lock()
	in := pr.stdin
	pr.mu.Unlock()
	if in == nil {
		return nil, errors.New("its stdin is closed (it exited, or close was called)")
	}
	_ = in.SetWriteDeadline(time.Now().Add(stdinWait))
	n, err := io.WriteString(in, data)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, fmt.Errorf("it read %d of %d bytes in %s: it is not reading its stdin", n, len(data), stdinWait)
		}
		return nil, fmt.Errorf("could not write to its stdin: %v", err)
	}
	return map[string]any{"status": "ok", "session_id": pr.id, "bytes": len(data)}, nil
}

func (pr *proc) closeStdin() (any, error) {
	pr.closeIn()
	return map[string]any{"status": "ok", "session_id": pr.id}, nil
}

// closeIn closes the runtime's end of stdin (the command reads EOF).
func (pr *proc) closeIn() {
	pr.mu.Lock()
	in := pr.stdin
	pr.stdin = nil
	pr.mu.Unlock()
	if in != nil {
		_ = in.Close()
	}
}

// fitOutput is s within n bytes: its start and end kept (two fifths, three fifths), the middle
// elided.
func fitOutput(s string, n int) string {
	if len(s) <= n {
		return s
	}
	h, t := n*2/5, n*3/5
	return strings.ToValidUTF8(s[:h], "") + fmt.Sprintf("\n… [%d bytes elided] …\n", len(s)-h-t) + strings.ToValidUTF8(s[len(s)-t:], "")
}

// ---- output -----------------------------------------------------------------------------------

// ansi is a terminal escape sequence (colours, cursor moves), dropped from what the model reads.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

// streamRedactor masks secrets in a stream: what arrives is redacted with what came before it,
// and the last overlap bytes are held back until more arrives (or flush), so a secret split
// across two writes is masked whole and never leaves in part.
type streamRedactor struct {
	redact  func(string) string
	overlap int
	emit    func(string)
	pending string
}

func (s *streamRedactor) write(b []byte) {
	text := s.pending + string(b)
	if s.redact != nil {
		text = s.redact(text)
	}
	cut := len(text) - s.overlap
	for cut > 0 && cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if cut <= 0 {
		s.pending = text
		return
	}
	s.emit(text[:cut])
	s.pending = text[cut:]
}

func (s *streamRedactor) flush() {
	text := s.pending
	if s.redact != nil {
		text = s.redact(text)
	}
	s.pending = ""
	if text != "" {
		s.emit(text)
	}
}

// ring is a process' output: its first quarter (head) and a rolling last three-quarters (tail),
// in stream offsets (written counts every byte ever written).
type ring struct {
	mu      sync.Mutex
	headMax int
	head    []byte
	tail    []byte // circular, len == tailMax once full
	tailMax int
	start   int // tail's oldest byte
	n       int // bytes in tail
	written int64
}

func newRing(size int) *ring {
	h := size / 4
	return &ring{headMax: h, tailMax: size - h, head: make([]byte, 0, min(h, 4096)), tail: make([]byte, size-h)}
}

func (r *ring) write(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := []byte(s)
	r.written += int64(len(b))
	if room := r.headMax - len(r.head); room > 0 {
		k := min(room, len(b))
		r.head = append(r.head, b[:k]...)
		b = b[k:]
	}
	for len(b) > 0 {
		if len(b) >= r.tailMax {
			copy(r.tail, b[len(b)-r.tailMax:])
			r.start, r.n = 0, r.tailMax
			return
		}
		end := (r.start + r.n) % r.tailMax
		k := min(len(b), r.tailMax-end)
		copy(r.tail[end:end+k], b[:k])
		b = b[k:]
		r.n += k
		if r.n > r.tailMax {
			over := r.n - r.tailMax
			r.start = (r.start + over) % r.tailMax
			r.n = r.tailMax
		}
	}
}

func (r *ring) total() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written
}

// tailFrom is the tail's bytes from stream offset off on; r.mu is held.
func (r *ring) tailFrom(off int64) []byte {
	first := r.written - int64(r.n)
	skip := int(max(0, off-first))
	if skip >= r.n {
		return nil
	}
	out := make([]byte, 0, r.n-skip)
	for i := skip; i < r.n; i++ {
		out = append(out, r.tail[(r.start+i)%r.tailMax])
	}
	return out
}

// text is all that is kept: the head, a note for the bytes dropped between, the tail.
func (r *ring) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	h := int64(len(r.head))
	first := r.written - int64(r.n)
	var b strings.Builder
	b.Write(r.head)
	if first > h {
		fmt.Fprintf(&b, "\n… [%d bytes elided] …\n", first-h)
	}
	b.Write(r.tailFrom(max(h, first)))
	return clean(b.String())
}

// since is the output from stream offset off to now, at most n bytes (its start and end when
// longer), the offset it reaches, and how many bytes it left out (the buffer had dropped them, or
// they did not fit).
func (r *ring) since(off int64, n int) (string, int64, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []byte
	var skipped int64
	h := int64(len(r.head))
	if off < h {
		out = append(out, r.head[off:]...)
		off = h
	}
	first := r.written - int64(r.n)
	if off < first {
		skipped += first - off
	}
	out = append(out, r.tailFrom(off)...)
	text := clean(string(out))
	if len(text) > n {
		skipped += int64(len(text) - n)
		text = fitOutput(text, n)
	}
	return text, r.written, skipped
}

// clean is output as the model reads it: valid UTF-8, no terminal escapes.
func clean(s string) string {
	return ansi.ReplaceAllString(strings.ToValidUTF8(s, "�"), "")
}
