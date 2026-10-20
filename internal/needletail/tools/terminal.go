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
			"Output (stdout and stderr together) is capped; long-running commands are killed at the timeout.",
		Schema: schema(`{"type":"object","properties":{
"command":{"type":"string","description":"The bash command to run."},
"timeout":{"type":"integer","description":"Seconds before the command is killed (default: the agent's command timeout)."},
"workdir":{"type":"string","description":"Directory to run in, inside the home or shared space (default: the home)."}},
"required":["command"]}`),
		Run: runTerminal,
	}
}

type terminalArgs struct {
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
	Workdir string `json:"workdir"`
}

type terminalResult struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

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
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "bash", "-lc", a.Command)
	cmd.Dir = dir
	cmd.Env = terminalEnv(env)
	cmd.Stdin = nil
	out := &capped{}
	cmd.Stdout, cmd.Stderr = out, out
	killGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	res := terminalResult{Output: out.String()}
	switch {
	case cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
		res.ExitCode, res.Error = -1, fmt.Sprintf("timed out after %s and was killed", timeout)
	case ctx.Err() != nil:
		res.ExitCode, res.Error = -1, "cancelled"
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

// capped keeps the first outputHead and the last outputTail bytes written.
type capped struct {
	mu      sync.Mutex
	head    []byte
	tail    []byte
	dropped int
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if room := outputHead - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		c.tail = append(c.tail, p...)
		if over := len(c.tail) - outputTail; over > 0 {
			c.dropped += over
			c.tail = append(c.tail[:0], c.tail[over:]...)
		}
	}
	return n, nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := string(c.head)
	if c.dropped > 0 {
		s += fmt.Sprintf("\n… [%d bytes of output elided] …\n", c.dropped)
	}
	return strings.ToValidUTF8(s+string(c.tail), "�")
}
