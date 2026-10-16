package link

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// MaxOutput caps what a capability may answer.
const MaxOutput = 1 << 20

// RunContainer runs a capability container: `docker run --rm -i` with input on stdin and env
// values passed through the environment (`-e NAME`), never on argv; its stdout is the answer.
func RunContainer(ctx context.Context, image string, command []string, env map[string]string, input []byte) ([]byte, error) {
	args := []string{"run", "--rm", "-i", "--network", "bridge", "--cap-drop", "ALL", "--security-opt", "no-new-privileges"}
	vars := os.Environ()
	for k, v := range env {
		args = append(args, "-e", k)
		vars = append(vars, k+"="+v)
	}
	args = append(append(args, image), command...)
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = vars
	cmd.Stdin = bytes.NewReader(input)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limited{&out, MaxOutput}, &limited{&errb, 16 << 10}
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

type limited struct {
	b *bytes.Buffer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.n - l.b.Len(); room > 0 {
		if len(p) > room {
			l.b.Write(p[:room])
		} else {
			l.b.Write(p)
		}
	}
	return len(p), nil
}
