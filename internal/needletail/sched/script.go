package sched

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/tools"
)

// maxScriptOutput bounds what is kept of each of a script's streams.
const maxScriptOutput = 1 << 20

// runScript runs one of the home's scripts (scripts/<path>; an absolute path must lie inside
// scripts/ too) as Hermes does: .sh and .bash with bash, .py and anything else with python3, the
// shebang ignored; cwd the script's own directory (Hermes': scripts/ for a script there), the
// terminal's environment, the script timeout, its whole
// process group killed at the end. It returns whether it exited 0 and its trimmed, redacted
// stdout, or a description of the failure (exit code, stderr, stdout).
func (s *Scheduler) runScript(ctx context.Context, script string) (bool, string) {
	path, err := s.scriptPath(script)
	if err != nil {
		return false, err.Error()
	}
	var argv []string
	switch strings.ToLower(filepath.Ext(path)) {
	case ".sh", ".bash":
		argv = []string{"bash", path}
	case ".luau":
		if s.o.Luau == nil {
			return false, fmt.Sprintf("%s: Luau is not available in this runtime", script)
		}
		// in the Luau sandbox, bounded by the script timeout (limits.script_timeout)
		return s.o.Luau(ctx, path, script)
	default:
		argv = []string{"python3", path}
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.ScriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = filepath.Dir(path)
	env := s.o.Env
	cmd.Env = tools.CommandEnv(&env)
	var stdout, stderr limited
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := tools.CommandContext(ctx, cmd, &env); err != nil {
		return false, err.Error()
	} // same quarantine jail as terminal
	cmd.WaitDelay = 2 * time.Second
	runErr := cmd.Run()
	out := strings.TrimSpace(s.o.Env.Redact(stdout.String()))
	errOut := strings.TrimSpace(s.o.Env.Redact(stderr.String()))
	switch {
	case cctx.Err() == context.DeadlineExceeded && ctx.Err() == nil:
		return false, fmt.Sprintf("Script timed out after %s: %s", s.o.ScriptTimeout, script)
	case ctx.Err() != nil:
		return false, "Script cancelled: the runtime is stopping"
	case errors.Is(runErr, exec.ErrWaitDelay):
		// exited 0 but left a background process holding its output: what it printed counts
	case runErr != nil:
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return false, fmt.Sprintf("Script failed to start: %v", runErr)
		}
		parts := []string{fmt.Sprintf("Script exited with code %d", ee.ExitCode())}
		if errOut != "" {
			parts = append(parts, "stderr:\n"+errOut)
		}
		if out != "" {
			parts = append(parts, "stdout:\n"+out)
		}
		return false, strings.Join(parts, "\n")
	}
	return true, out
}

// scriptPath resolves a job's script inside the home's scripts/, symlinks included.
func (s *Scheduler) scriptPath(script string) (string, error) {
	dir := filepath.Join(s.o.Home, "scripts")
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("script %s: the home has no scripts/ directory", script)
	}
	p := script
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("script not found: %s (scripts are looked up in the home's scripts/)", script)
	}
	if rel, err := filepath.Rel(realDir, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("refused: script %s resolves outside the home's scripts/", script)
	}
	if st, err := os.Stat(real); err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("script %s is not a regular file", script)
	}
	return real, nil
}

// limited keeps the first maxScriptOutput bytes written.
type limited struct {
	mu  sync.Mutex
	buf bytes.Buffer
	cut bool
}

func (l *limited) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := maxScriptOutput - l.buf.Len(); room > 0 {
		l.buf.Write(p[:min(room, len(p))])
	}
	if l.buf.Len() >= maxScriptOutput && len(p) > 0 {
		l.cut = true
	}
	return len(p), nil
}

func (l *limited) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := strings.ToValidUTF8(l.buf.String(), "�")
	if l.cut {
		s += "\n... [output truncated]"
	}
	return s
}
