package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

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
			"Use it for scripts, git, curl, python3 and anything the file tools do not cover. " +
			"Output (stdout and stderr together) is capped; long-running commands are killed at the timeout. " +
			"A process left running in the background (cmd &) keeps running after the command returns; its later output is not captured. " +
			"To run a skill's scripts, pass skill: SKILL_DIR is then set to its directory (use \"$SKILL_DIR\"/scripts/…).",
		Schema: schema(`{"type":"object","properties":{
"command":{"type":"string","description":"The bash command to run."},
"timeout":{"type":"integer","description":"Seconds before the command is killed (default: the agent's command timeout)."},
"workdir":{"type":"string","description":"Directory to run in, inside the home or shared space (default: the home)."},
"skill":{"type":"string","description":"The skill this command runs from (its name or directory): sets SKILL_DIR."}},
"required":["command"]}`),
		Run: runTerminal,
	}
}

type terminalArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
	Workdir string `json:"workdir"`
	Skill   string `json:"skill"`
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
	timeout := env.CommandTimeout
	if a.Timeout > 0 {
		timeout = time.Duration(a.Timeout) * time.Second
	}
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	timeout = min(timeout, maxTimeout)
	dir := env.Home
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
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bash", "-lc", a.Command)
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
	killGroup(cmd)
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

// fromSkill is the directory of the skill a command runs from ("" when none), with its use
// counted: the skill named by the call, else the one skill whose directory the command names,
// else, for a command that uses SKILL_DIR, the skill the session last viewed. A command that uses
// SKILL_DIR with no skill to give it is refused rather than run with it empty.
func fromSkill(env *Env, a terminalArgs) (string, error) {
	uses := strings.Contains(a.Command, skills.DirVar)
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

// CommandEnv is the environment a command the runtime starts gets (the terminal's, a schedule's
// script): the scrubbed process env with HOME set to the agent's home.
func CommandEnv(env *Env) []string { return terminalEnv(env) }

// KillGroup runs cmd in its own process group, killed whole when cmd is cancelled (unix).
func KillGroup(cmd *exec.Cmd) { killGroup(cmd) }

// terminalEnv is the scrubbed process env with HOME set to the agent's home.
func terminalEnv(env *Env) []string {
	var base []string
	if env.Environ != nil {
		base = env.Environ()
	} else {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if !strings.HasPrefix(kv, "HOME=") {
			out = append(out, kv)
		}
	}
	return append(out, "HOME="+env.Home)
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
