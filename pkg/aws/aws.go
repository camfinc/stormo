// Package aws is a thin wrapper over the aws CLI (uses whatever credentials/profile the shell has).
// Stormo prints and runs the same commands an operator would, so every AWS change is reviewable.
package aws

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Result of one aws CLI call.
type Result struct {
	Code           int
	Stdout, Stderr string
}

// CLI runs the aws CLI. Secret values are passed on stdin, never on argv (visible in `ps`).
type CLI func(args []string, stdin *string) Result

// Exec is the real aws CLI.
func Exec(args []string, stdin *string) Result {
	cmd := exec.Command("aws", args...)
	if stdin != nil {
		cmd.Stdin = strings.NewReader(*stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	code := 0
	if err := cmd.Run(); err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			errb.WriteString(err.Error())
		}
	}
	return Result{code, out.String(), errb.String()}
}

// JSON runs a call with --output json and decodes it, or fails with stderr.
func JSON(cli CLI, args []string, v any) error {
	r := cli(append(append([]string{}, args...), "--output", "json"), nil)
	if r.Code != 0 {
		n := min(2, len(args))
		return fmt.Errorf("aws %s failed: %s", strings.Join(args[:n], " "), strings.TrimSpace(r.Stderr))
	}
	body := r.Stdout
	if body == "" {
		body = "{}"
	}
	return json.Unmarshal([]byte(body), v)
}
