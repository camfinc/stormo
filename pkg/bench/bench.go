// Package bench runs scenario files (agents/<id>/bench/*.yaml): one prompt plus assertions on the
// reply. The bench is the regression gate for learning: run it on the baseline built from a
// dream's accepted proposals before committing them.
package bench

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/shared"
	"go.yaml.in/yaml/v3"
)

// Expectation is what a reply must (not) do.
type Expectation struct {
	Contains    []string `yaml:"contains" json:"contains,omitempty"`
	NotContains []string `yaml:"not_contains" json:"not_contains,omitempty"`
	// Regex, case-insensitive (RE2 syntax: no lookarounds or backreferences).
	Matches    string   `yaml:"matches" json:"matches,omitempty"`
	NotMatches string   `yaml:"not_matches" json:"not_matches,omitempty"`
	MaxChars   int      `yaml:"max_chars" json:"max_chars,omitempty"`
	Tools      []string `yaml:"tools" json:"tools,omitempty"` // tool names that must be called
	NoTools    []string `yaml:"no_tools" json:"no_tools,omitempty"`
	Silent     bool     `yaml:"silent" json:"silent,omitempty"` // reply must be empty (overheard chatter)
}

// Mock is the canned reply for the mock engine (CI without model access).
type Mock struct {
	Text  string   `yaml:"text" json:"text"`
	Tools []string `yaml:"tools" json:"tools,omitempty"`
}

// Scenario is one bench file.
type Scenario struct {
	ID          string      `yaml:"id" json:"id"`
	Description string      `yaml:"description" json:"description,omitempty"`
	Prompt      string      `yaml:"prompt" json:"prompt"`
	Expect      Expectation `yaml:"expect" json:"expect"`
	Mock        *Mock       `yaml:"mock" json:"mock,omitempty"`
	Tags        []string    `yaml:"tags" json:"tags,omitempty"`
}

// Result of one scenario.
type Result struct {
	ID       string          `json:"id"`
	OK       bool            `json:"ok"`
	Failures []string        `json:"failures"`
	Run      engine.BenchRun `json:"run"`
}

// LoadScenarios reads agents/<id>/bench/*.yaml, sorted by id.
func LoadScenarios(root, agent string) ([]Scenario, error) {
	dir := filepath.Join(manifest.AgentDir(root, agent), "bench")
	paths, _ := filepath.Glob(filepath.Join(dir, "*.yaml"))
	out := []Scenario{}
	for _, p := range paths {
		body, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var s Scenario
		if err := yaml.Unmarshal(body, &s); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if s.ID == "" {
			s.ID = strings.TrimSuffix(filepath.Base(p), ".yaml")
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Check returns the failed assertions (empty when the run passes).
func Check(s Scenario, run engine.BenchRun) []string {
	f := []string{}
	e := s.Expect
	t := run.Text
	lower := strings.ToLower(t)
	if run.ExitCode != 0 {
		msg := fmt.Sprintf("exit code %d", run.ExitCode)
		if run.Error != "" {
			msg += ": " + run.Error
		}
		f = append(f, msg)
	}
	for _, c := range e.Contains {
		if !strings.Contains(lower, strings.ToLower(c)) {
			f = append(f, `missing "`+c+`"`)
		}
	}
	for _, c := range e.NotContains {
		if strings.Contains(lower, strings.ToLower(c)) {
			f = append(f, `contains forbidden "`+c+`"`)
		}
	}
	match := func(pattern string) (bool, error) {
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			return false, err
		}
		return re.MatchString(t), nil
	}
	if e.Matches != "" {
		if ok, err := match(e.Matches); err != nil {
			f = append(f, fmt.Sprintf("invalid regex /%s/: %v", e.Matches, err))
		} else if !ok {
			f = append(f, fmt.Sprintf("does not match /%s/", e.Matches))
		}
	}
	if e.NotMatches != "" {
		if ok, err := match(e.NotMatches); err != nil {
			f = append(f, fmt.Sprintf("invalid regex /%s/: %v", e.NotMatches, err))
		} else if ok {
			f = append(f, fmt.Sprintf("matches forbidden /%s/", e.NotMatches))
		}
	}
	if n := learning.UTF16Len(t); e.MaxChars > 0 && n > e.MaxChars {
		f = append(f, fmt.Sprintf("%d chars > %d", n, e.MaxChars))
	}
	for _, tool := range e.Tools {
		if !slices.Contains(run.Tools, tool) {
			f = append(f, fmt.Sprintf("tool %s not called", tool))
		}
	}
	for _, tool := range e.NoTools {
		if slices.Contains(run.Tools, tool) {
			f = append(f, fmt.Sprintf("tool %s called", tool))
		}
	}
	if e.Silent && strings.TrimSpace(t) != "" {
		f = append(f, "expected no reply")
	}
	return f
}

// Runner executes one scenario.
type Runner func(s Scenario) (engine.BenchRun, error)

// MockRunner replies with each scenario's canned mock.
func MockRunner(s Scenario) (engine.BenchRun, error) {
	if s.Mock == nil {
		return engine.BenchRun{Tools: []string{}, ExitCode: 1, Error: "scenario has no mock reply"}, nil
	}
	tools := s.Mock.Tools
	if tools == nil {
		tools = []string{}
	}
	return engine.BenchRun{Text: s.Mock.Text, Tools: tools}, nil
}

// DockerRunner runs each scenario in a throwaway container of the pinned engine image, with a copy
// of the compiled baseline as its home and secrets from a local env file. Requires Docker and keys.
func DockerRunner(a *manifest.Agent, eng engine.Engine, baselineDir, envFile, sharedDir string) Runner {
	return func(s Scenario) (engine.BenchRun, error) {
		home, err := os.MkdirTemp("", "swarm-bench-"+a.ID+"-")
		if err != nil {
			return engine.BenchRun{}, err
		}
		defer os.RemoveAll(home)
		if err := os.CopyFS(home, os.DirFS(baselineDir)); err != nil {
			return engine.BenchRun{}, err
		}
		argv := []string{"docker", "run", "--rm", "--env-file", envFile}
		for _, kv := range eng.Env(a, manifest.Local).Pairs() {
			argv = append(argv, "-e", kv.Name+"="+kv.Value)
		}
		argv = append(argv, "-v", home+":"+eng.Layout().Home)
		// Same two layers the ECS task mounts, backed by a local directory that persists across runs.
		for _, l := range shared.Layers(a) {
			dir := filepath.Join(sharedDir, l.Layer)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return engine.BenchRun{}, err
			}
			argv = append(argv, "-v", dir+":"+l.Path)
		}
		argv = append(append(argv, eng.Image(a)), eng.BenchArgv(s.Prompt)...)
		cmd := exec.Command(argv[0], argv[1:]...)
		var out bytes.Buffer
		cmd.Stdout = &out
		code := 0
		if err := cmd.Run(); err != nil {
			ee, ok := err.(*exec.ExitError)
			if !ok {
				return engine.BenchRun{}, err
			}
			code = ee.ExitCode()
		}
		return eng.ParseBench(out.String(), code), nil
	}
}

// Run executes the scenarios in order.
func Run(scenarios []Scenario, runner Runner) ([]Result, error) {
	out := []Result{}
	for _, s := range scenarios {
		run, err := runner(s)
		if err != nil {
			return out, fmt.Errorf("%s: %w", s.ID, err)
		}
		failures := Check(s, run)
		out = append(out, Result{ID: s.ID, OK: len(failures) == 0, Failures: failures, Run: run})
	}
	return out, nil
}
