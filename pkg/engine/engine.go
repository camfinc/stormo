// Package engine is the contract between Stormo and an agent runtime (Hermes today): how to
// compile a baseline home from a manifest, run it, and classify every runtime file for naps.
package engine

import (
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/camfinc/stormo/pkg/env"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// FileClass is how a runtime file is treated by naps:
//   - learning: what the agent taught itself; harvested by dream into git proposals after scrub+review.
//   - state:    durable tool/app state; persisted and restored, never harvested into git.
//   - raw:      conversation history (PII); persisted under a separate prefix, restored on boot,
//     never readable by dream.
type FileClass string

const (
	Learning FileClass = "learning"
	State    FileClass = "state"
	Raw      FileClass = "raw"
)

// SnapshotRule classifies runtime files; the first matching rule wins.
type SnapshotRule struct {
	Class FileClass `json:"class"`
	// Glob relative to the engine home (doublestar: `**`, `{a,b}`, `[!.]`).
	Glob string `json:"glob"`
	// SQLite database: snapshot with VACUUM INTO (consistent under WAL), skip -wal/-shm siblings.
	SQLite bool `json:"sqlite,omitempty"`
}

// Layout is where things live inside the engine home.
type Layout struct {
	// Engine home inside the agent container (shared task volume).
	Home  string         `json:"home"`
	Rules []SnapshotRule `json:"rules"`
	// Hot-memory files relative to home.
	Memory struct{ Memory, User string } `json:"memory"`
	// Skills root relative to home.
	SkillsDir string `json:"skillsDir"`
	// Root for manifest `state:` entries relative to home; each gets <StateRoot>/<name>.
	StateRoot string `json:"stateRoot"`
	// uid/gid the engine runs as; rehydrate (root) hands the home to it so the agent can write.
	RuntimeUID, RuntimeGID int
}

// Classify returns the first rule matching path (relative to the home), or nil.
func Classify(path string, rules []SnapshotRule) *SnapshotRule {
	for i := range rules {
		if Match(rules[i].Glob, path) {
			return &rules[i]
		}
	}
	return nil
}

// Match is a glob match where a trailing "/**" needs at least one more path segment: "skills/x/**"
// claims files inside skills/x/, never a file named skills/x. (doublestar alone lets "**" match
// zero segments; the rules are written for the stricter meaning, pinned by testdata/classify.json.)
func Match(glob, path string) bool {
	if strings.HasSuffix(glob, "/**") {
		glob = strings.TrimSuffix(glob, "**") + "*/**"
	}
	ok, _ := doublestar.Match(glob, path)
	return ok
}

// CompileContext is what the build hands an engine.
type CompileContext struct {
	Instance *instance.Instance
	// Where the baseline will run; local applies the manifest's engine.local model.
	Target manifest.Target
	// Hot-tier seed entries, ranked, already within budget.
	Seed struct{ Memory, User []string }
	// Cold-tier knowledge skill files (path relative to the skills dir → content).
	Knowledge map[string]string
	// Skill dirs (relative to the agent's skills/) left out: an optional secret they need is absent.
	SkipSkills []string
}

// BenchRun is one bench turn's parsed result.
type BenchRun struct {
	Text     string   `json:"text"`
	Tools    []string `json:"tools"`
	ExitCode int      `json:"exitCode"`
	Tokens   *int     `json:"tokens,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// Engine turns a manifest into a baseline home directory and knows its runtime layout.
type Engine interface {
	Kind() string
	Layout() *Layout
	Image(a *manifest.Agent) string
	// Compile returns baseline files relative to the engine home (SOUL, config, skills, plugins,
	// cron, seed memory).
	Compile(a *manifest.Agent, ctx CompileContext) (map[string][]byte, error)
	// Env for the agent container (non-secret), for ECS or a local container, in a stable order.
	Env(a *manifest.Agent, target manifest.Target) *env.Env
	Command(a *manifest.Agent) []string
	Port() int
	HealthCheck() []string
	// EngineOwnedSkills are skills the engine ships itself; naps keep them but rehydrate lets the
	// image own them.
	EngineOwnedSkills(home string) (map[string]bool, error)
	// BenchArgv is one bench turn executed inside the agent image; ParseBench reads its output.
	BenchArgv(prompt string) []string
	ParseBench(stdout string, exitCode int) BenchRun
}

// IsTempSibling reports SQLite journal files that naps never copy.
func IsTempSibling(path string) bool {
	return strings.HasSuffix(path, "-wal") || strings.HasSuffix(path, "-shm") || strings.HasSuffix(path, "-journal")
}
