package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/jail"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/skills"
)

// Terminal output is capped: the head and the tail are kept, the middle is elided.
const (
	outputHead = 20_000
	outputTail = 30_000
	maxTimeout = 3600 * time.Second
)

func terminalTool() Tool {
	return Tool{
		Name: "terminal",
		Description: "Run a shell command with bash in the agent's home (the working directory). " +
			"Use it for scripts, git, curl, jq, python3 and anything the file tools do not cover. " +
			"Output (stdout and stderr together) is capped; long-running commands are killed at the timeout. " +
			"For a server, a watcher or a long job, pass background: true: it starts and returns a session_id at once; " +
			"follow it with process_manage (poll, wait, log, write, kill). A process left running by a normal command (cmd &) is not tracked: its later output is lost. " +
			"To run a skill's scripts, pass skill: SKILL_DIR is then set to its directory (use \"$SKILL_DIR\"/scripts/…).",
		Schema: schema(`{"type":"object","properties":{
"command":{"type":"string","description":"The bash command to run."},
"timeout":{"type":"integer","description":"Seconds before the command is killed (default: the agent's command timeout)."},
"workdir":{"type":"string","description":"Directory to run in, inside the home or shared space (default: the home)."},
"skill":{"type":"string","description":"The skill this command runs from (its name or directory): sets SKILL_DIR."},
"background":{"type":"boolean","description":"Start it in the background and return its session_id at once (timeout does not apply; it runs until it exits, is killed or reaches its lifetime)."}},
"required":["command"]}`),
		Run: runTerminal,
	}
}

type terminalArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
	Workdir string `json:"workdir"`
	Skill   string `json:"skill"`
	// Background starts the command in the background (background.go).
	Background bool `json:"background"`
}

type terminalResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
	Note     string `json:"note,omitempty"`
}

// noteBackground: the command exited but a process it left in the background still held the
// output open. That process is not killed (an agent may start a server on purpose).
const noteBackground = "background processes are still running; output they write from now on is not captured"

func runTerminal(ctx context.Context, env *Env, raw json.RawMessage) (any, error) {
	var a terminalArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Command) == "" {
		return nil, errors.New("command is empty")
	}
	// a risky command (approvals.commands) needs a person, in any conversation; a scheduled run has
	// none, so there it is refused. An approved call (endorsed) runs as it was approved.
	if rule := riskyRule(env, a.Command); rule != "" && messaging.Endorsed(ctx) == "" {
		if messaging.Scheduled(ctx) {
			return nil, fmt.Errorf("refused: the command matches the risky-command rule %q, and a scheduled run has no person to approve it", rule)
		}
		return nil, messaging.NeedsApprovalRisky("the terminal command", "it matches the risky-command rule \""+rule+"\"")
	}
	if a.Background && UntrustedWhy(ctx, env) != "" && conversationOf(env) == "" {
		return nil, messaging.Deferred("a background process without a known conversation", UntrustedWhy(ctx, env))
	}
	var prepErr error
	env, prepErr = quarantineEnv(ctx, env)
	if prepErr != nil {
		return nil, prepErr
	}
	timeout := env.CommandTimeout
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
	}
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	timeout = min(timeout, maxTimeout)
	dir := env.Home
	if env.WorkDir != "" {
		dir = env.WorkDir // a tainted run's commands start where they may write
	}
	if a.Workdir != "" {
		d, err := env.Sandbox.Resolve(a.Workdir)
		if err != nil {
			return nil, err
		}
		dir = d
	}
	skillDir, err := fromSkill(env, a)
	if err != nil {
		return nil, err
	}
	if a.Background {
		return startBackground(env, a, dir, skillDir)
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Not a login shell and no rc file: nothing in the home (.bash_profile, .profile, .bashrc, which
	// a Luau tool or a fetched file could plant) runs before the command. The environment is
	// explicit instead (terminalEnv).
	cmd := exec.CommandContext(cctx, "bash", "--noprofile", "--norc", "-c", a.Command)
	cmd.Dir = dir
	cmd.Env = terminalEnv(env)
	if skillDir != "" {
		cmd.Env = append(cmd.Env, skills.DirVar+"="+skillDir)
	}
	cmd.Stdin = nil
	out := &capped{}
	if env.LongestSecret != nil {
		// twice the longest: a mask can be longer than what it replaces
		out.overlap = 2 * env.LongestSecret()
	}
	cmd.Stdout, cmd.Stderr = out, out
	command(cmd, env)
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	res := terminalResult{Output: out.String(env.Redact)}
	switch {
	case cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
		res.ExitCode, res.Error = -1, fmt.Sprintf("timed out after %s and was killed", timeout)
	case ctx.Err() != nil:
		res.ExitCode, res.Error = -1, "cancelled"
	case errors.Is(err, exec.ErrWaitDelay):
		// Go reports this only for a command that otherwise exited 0
		res.ExitCode, res.Note = cmd.ProcessState.ExitCode(), noteBackground
	case err != nil:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			res.ExitCode = ee.ExitCode()
		} else {
			return nil, err
		}
	}
	return res, nil
}

func riskyRule(env *Env, command string) string {
	if env == nil || env.Risky == nil {
		return ""
	}
	return env.Risky(command)
}

// usesDirVar matches a command that expands SKILL_DIR ($SKILL_DIR or ${SKILL_DIR}), not one that
// only names it (grep -r SKILL_DIR skills).
var usesDirVar = regexp.MustCompile(`\$\{?` + skills.DirVar + `\b`)

// fromSkill is the directory of the skill a command runs from ("" when none), with its use
// counted: the skill named by the call, else the one skill whose directory the command names,
// else, for a command that uses SKILL_DIR, the skill the session last viewed. A command that uses
// SKILL_DIR with no skill to give it is refused rather than run with it empty.
func fromSkill(env *Env, a terminalArgs) (string, error) {
	uses := usesDirVar.MatchString(a.Command)
	if env.Skills == nil {
		if a.Skill != "" {
			return "", errors.New("skills are not available")
		}
		return "", nil
	}
	var s skills.Skill
	var ok bool
	switch {
	case strings.TrimSpace(a.Skill) != "":
		found, err := env.Skills.Find(a.Skill)
		if err != nil {
			return "", err
		}
		s, ok = found, true
	default:
		if s, ok = env.Skills.Referenced(a.Command); !ok && uses {
			s, ok = env.Skills.LastViewed(env.Session)
		}
	}
	if !ok {
		if uses {
			return "", errors.New("the command uses SKILL_DIR but names no skill: pass skill (its name)")
		}
		return "", nil
	}
	env.Skills.Used(s)
	return env.Skills.Abs(s), nil
}

// DefaultPath is the PATH a command gets when the runtime's env has none: what the image's login
// profile set (Alpine's /etc/profile) when the terminal was a login shell.
const DefaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// CommandEnv is the environment a command the runtime starts gets (the terminal's, a schedule's
// script): the scrubbed process env with HOME set to the agent's home and git trusting every
// repository (trustRepos).
func CommandEnv(env *Env) []string { return terminalEnv(env) }

// Command prepares cmd as one the agent runs (a schedule's script, as the terminal's): its own
// process group, killed whole when cmd is cancelled, env's Agent user (the uid split) and env's
// Jail. cmd's path, arguments and environment are set before.
func Command(cmd *exec.Cmd, env *Env) { command(cmd, env) }

// CommandContext prepares a context-aware command, refusing quarantine when the
// filesystem policy cannot be enforced. Callers must check the error before Start.
func CommandContext(ctx context.Context, cmd *exec.Cmd, env *Env) error {
	prepared, err := quarantineEnv(ctx, env)
	if err != nil {
		return err
	}
	if prepared != env {
		kept := cmd.Env[:0:0]
		for _, kv := range cmd.Env {
			if !dropForTainted(prepared, kv) && !strings.HasPrefix(kv, "TMPDIR=") && !strings.HasPrefix(kv, "TMP=") && !strings.HasPrefix(kv, "TEMP=") {
				kept = append(kept, kv)
			}
		}
		cmd.Env = append(kept, "TMPDIR="+prepared.TmpDir, "TMP="+prepared.TmpDir, "TEMP="+prepared.TmpDir)
	}
	command(cmd, prepared)
	return nil
}

// quarantineEnv is env for a tainted run's command (docs/needletail.md §7): jailed read-only but
// for the conversation's work directory (work/<conversation>, its TMPDIR inside), started there,
// without the core's credentials or the Luau http_secrets in its environment. Without a jail that
// can hold it (Landlock ABI 3 or newer: truncation too) the command is refused: fail closed.
func quarantineEnv(ctx context.Context, env *Env) (*Env, error) {
	why := UntrustedWhy(ctx, env)
	if why == "" {
		return env, nil
	}
	if env.Jail == nil {
		return nil, messaging.Deferred("terminal execution: a read-only Landlock jail is unavailable", why)
	}
	abi, err := jail.ABI()
	if err != nil || abi < 3 {
		return nil, messaging.Deferred("terminal execution: a tainted run requires Landlock ABI 3 or newer (including truncation protection)", why)
	}
	return readOnlyCommandEnv(env, why)
}

// readOnlyCommandEnv copies the launcher, so a tainted background command retains its
// restriction and cannot alter another trusted command's policy.
func readOnlyCommandEnv(env *Env, why string) (*Env, error) {
	home, err := filepath.Abs(env.Home)
	if err != nil {
		return nil, messaging.Deferred("terminal execution: invalid home", why)
	}
	jailHome, err := filepath.Abs(env.Jail.Spec.Home)
	if err != nil || filepath.Clean(jailHome) != filepath.Clean(home) {
		return nil, messaging.Deferred("terminal execution: the jail does not match the home", why)
	}
	work, err := EnsureWork(env)
	if err != nil {
		return nil, messaging.Deferred("terminal execution: the conversation's work directory: "+err.Error(), why)
	}
	copy := *env
	launcher := *env.Jail
	launcher.Spec = env.Jail.Spec
	launcher.Spec.ReadOnlyHome = true
	launcher.Spec.Writable = []string{work}
	copy.Jail = &launcher
	copy.TmpDir = filepath.Join(work, ".tmp")
	copy.WorkDir = work
	copy.tainted = true
	return &copy, nil
}

// command is Command; on unix it also jails cmd (env.Jail).
func command(cmd *exec.Cmd, env *Env) {
	start(cmd, env.Agent)
	if env.Jail != nil {
		env.Jail.Wrap(cmd)
	}
}

// terminalEnv is the scrubbed process env with HOME set to the agent's home and an explicit PATH:
// the env's absolute entries, else DefaultPath. A relative entry ("", ".", "bin") would run what
// is in the command's directory, the home, by a bare command name. With env.TmpDir, TMPDIR, TMP
// and TEMP are it.
func terminalEnv(env *Env) []string {
	var base []string
	if env.Environ != nil {
		base = env.Environ()
	} else {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+2)
	path := ""
	for _, kv := range base {
		if env.tainted && dropForTainted(env, kv) {
			continue // a tainted run's command gets no credential to act elsewhere as the agent
		}
		switch {
		case strings.HasPrefix(kv, "HOME="):
		case strings.HasPrefix(kv, "PATH="):
			path = absolutePath(strings.TrimPrefix(kv, "PATH="))
		case env.TmpDir != "" && (strings.HasPrefix(kv, "TMPDIR=") || strings.HasPrefix(kv, "TMP=") || strings.HasPrefix(kv, "TEMP=")):
		default:
			out = append(out, kv)
		}
	}
	if path == "" {
		path = DefaultPath
	}
	out = append(trustRepos(out), "PATH="+path, "HOME="+env.Home)
	if env.TmpDir != "" {
		out = append(out, "TMPDIR="+env.TmpDir, "TMP="+env.TmpDir, "TEMP="+env.TmpDir)
	}
	return out
}

// trustRepos adds safe.directory=* to git's config through the environment (GIT_CONFIG_COUNT,
// GIT_CONFIG_KEY_<n>, GIT_CONFIG_VALUE_<n>), after the entries env already sets that way. Under
// the uid split the agent's commands work in directories the runtime made (a new skill's, the
// ones write_file made, the home itself; on ECS all of /shared, which EFS stores as 10000), and
// git refuses a repository another user owns ("dubious ownership"). A count git could not read
// (not a number, negative) is replaced with its entries.
func trustRepos(env []string) []string {
	n, at := 0, -1
	for i, kv := range env {
		if v, ok := strings.CutPrefix(kv, "GIT_CONFIG_COUNT="); ok {
			at = i
			if c, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && c >= 0 {
				n = c
			} else {
				n = -1
			}
		}
	}
	out := env[:0:0]
	for i, kv := range env {
		switch {
		case i == at:
		case strings.HasPrefix(kv, "GIT_CONFIG_COUNT="):
		case n < 0 && (strings.HasPrefix(kv, "GIT_CONFIG_KEY_") || strings.HasPrefix(kv, "GIT_CONFIG_VALUE_")):
		default:
			out = append(out, kv)
		}
	}
	n = max(n, 0)
	i := strconv.Itoa(n)
	return append(out, "GIT_CONFIG_COUNT="+strconv.Itoa(n+1), "GIT_CONFIG_KEY_"+i+"=safe.directory", "GIT_CONFIG_VALUE_"+i+"=*")
}

// dropForTainted: kv is a credential a tainted run's command must not hold (Env.TaintedDrop: the
// core's URL and key, which would let it write to the core directly, and the Luau http_secrets).
func dropForTainted(env *Env, kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	for _, n := range env.TaintedDrop {
		if name == n {
			return true
		}
	}
	return false
}

// absolutePath is a PATH value with its relative entries dropped.
func absolutePath(p string) string {
	var keep []string
	for _, d := range strings.Split(p, ":") {
		if strings.HasPrefix(d, "/") {
			keep = append(keep, d)
		}
	}
	return strings.Join(keep, ":")
}

// capped keeps the first outputHead and the last outputTail bytes written, each with overlap
// bytes more, so that a secret straddling a cut can be masked whole (see String).
type capped struct {
	overlap int

	mu      sync.Mutex
	head    []byte
	tail    []byte
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if room := outputHead + c.overlap - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		c.tail = append(c.tail, p...)
		if over := len(c.tail) - outputTail - c.overlap; over > 0 {
			c.dropped += over
			c.tail = append(c.tail[:0], c.tail[over:]...)
		}
	}
	return n, nil
}

// String is the output kept. When the middle was elided, the head and the tail are masked with
// redact first and only then cut by overlap bytes: a secret that starts in the kept head ends
// within its overlap, so it is masked whole, and only the overlap can hold part of one (one the
// buffer cut), which the cut drops. The same holds at the tail's start.
func (c *capped) String(redact func(string) string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dropped == 0 {
		return strings.ToValidUTF8(string(c.head)+string(c.tail), "�")
	}
	if redact == nil {
		redact = func(s string) string { return s }
	}
	head, tail := string(c.head), string(c.tail)
	if c.overlap > 0 {
		head, tail = redact(head), redact(tail)
		head = head[:max(0, len(head)-c.overlap)]
		tail = tail[min(len(tail), c.overlap):]
	}
	s := head + fmt.Sprintf("\n… [%d bytes of output elided] …\n", c.dropped+2*c.overlap) + tail
	return strings.ToValidUTF8(s, "�")
}
