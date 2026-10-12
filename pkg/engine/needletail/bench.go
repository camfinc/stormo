package needletail

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/camfinc/stormo/pkg/engine"
)

// BenchArgv is one oneshot turn in the image.
func (n *Needletail) BenchArgv(prompt string) []string {
	return []string{"needletail", "oneshot", "--format", "stream-json", "-q", prompt}
}

// ParseBench reads oneshot's stream-json: tool_use, text and result lines, as Hermes writes them.
func (n *Needletail) ParseBench(stdout string, exitCode int) engine.BenchRun {
	run := engine.BenchRun{Tools: []string{}, ExitCode: exitCode}
	var text string
	var result *struct {
		Text     *string `json:"text"`
		ExitCode *int    `json:"exit_code"`
		Tokens   *struct {
			Total *int `json:"total"`
		} `json:"tokens"`
		Error string `json:"error"`
	}
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Name any    `json:"name"`
			Text string `json:"text"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "tool_use":
			run.Tools = append(run.Tools, fmt.Sprint(ev.Name))
		case "text":
			text += ev.Text
		case "result":
			result = nil
			_ = json.Unmarshal([]byte(line), &result)
		}
	}
	run.Text = text
	if result != nil {
		if result.Text != nil {
			run.Text = *result.Text
		}
		if result.ExitCode != nil {
			run.ExitCode = *result.ExitCode
		}
		if result.Tokens != nil && result.Tokens.Total != nil {
			t := *result.Tokens.Total
			run.Tokens = &t
		}
		run.Error = result.Error
	}
	return run
}
