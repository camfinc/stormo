// Package needletail is the needletail engine (docs/needletail.md): a Go runtime that serves the
// same home layout, memory format and schedules file as Hermes, and lets agents write sandboxed Luau
// tools inside their skills. This package is the adapter Stormo sees; the runtime's wire formats are
// internal/needletail/wire.
package needletail

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/shared"
)

const (
	home = "/opt/data"
	port = 8642
	// Repository is the image's name. Where it is published (a registry prefix) is an open
	// question for the owner (docs/needletail.md §11); until then it is the locally built name.
	Repository = "needletail"
	// ConfigFile is the agent's needletail-only settings (Luau grants), read only by the adapter.
	ConfigFile = "engine/needletail/config.yaml"
)

// Needletail is the engine for one instance.
type Needletail struct {
	inst   *instance.Instance
	layout engine.Layout
}

// New builds the engine for an instance.
func New(inst *instance.Instance) *Needletail {
	sd := inst.Names.StateDir
	l := engine.Layout{
		Home:       home,
		SkillsDir:  "skills",
		StateRoot:  sd,
		RuntimeUID: shared.PosixUID, RuntimeGID: shared.PosixUID,
		// The runtime writes only to the home and /shared, and its commands' TMPDIR is in the
		// home: the container needs no writable root and no tmpfs (docs/needletail.md §7).
		ReadOnlyRoot: true,
		Rules: []engine.SnapshotRule{
			// learning: what the agent taught itself (Luau tools live inside skills)
			{Class: engine.Learning, Glob: "memories/{MEMORY,USER}.md"},
			{Class: engine.Learning, Glob: "skills/.usage.json"},
			{Class: engine.Learning, Glob: "skills/[!.]*/**"},
			{Class: engine.Learning, Glob: "cron/jobs.json"},
			// Job scripts and the modules they import: the repo's and any the agent wrote or edited
			// for its own jobs (Layout.ScriptsDir), so a job never outlives the script it runs and
			// goes to review with it. Caches are left out, as the build leaves them out.
			{Class: engine.Learning, Glob: "scripts/**", Except: []string{"**/__pycache__/**", "**/node_modules/**", "**/.*", "**/.*/**", "**/*.pyc"}},
			// The rendered config is rebuilt from the repo on every boot (as Hermes' config.yaml):
			// never restored from a nap, so a changed agent.yaml or grant reaches the agent on its
			// next start, and nothing the agent writes into it survives one.
			{Class: engine.Learning, Glob: "needletail.yaml"},
			// raw: conversation history and the Luau audit log (PII)
			{Class: engine.Raw, Glob: "sessions/**"},
			// the memory archive (every record, hot or not: client data): restored verbatim and
			// never read by the dream, which learns from the hot files alone
			{Class: engine.Raw, Glob: "memories/RECORDS.md"},
			{Class: engine.Raw, Glob: "logs/audit/**"},
			// a monitor's last output and each run's record hold what jobs fetched and replied:
			// client data, before the state rule that would claim them
			{Class: engine.Raw, Glob: "state/needletail/jobs/**"},
			{Class: engine.Raw, Glob: "state/needletail/monitor/**"},
			// the pending follow-ups hold what the agent promised to say or do in a conversation:
			// client data, restored at boot so they survive a restart, never read by the dream
			{Class: engine.Raw, Glob: "state/needletail/followups.json"},
			// state: tool state and the runtime's own state
			{Class: engine.State, Glob: sd + "/**"},
			{Class: engine.State, Glob: "state/needletail/**"},
		},
	}
	l.Memory.Memory, l.Memory.User = "memories/MEMORY.md", "memories/USER.md"
	l.SkillUsage = "skills/.usage.json"
	l.Managed = []string{"skills", "scripts"}
	l.ScriptsDir = "scripts"
	l.Schedules = "cron/jobs.json"
	return &Needletail{inst: inst, layout: l}
}

func (n *Needletail) Kind() string           { return "needletail" }
func (n *Needletail) Layout() *engine.Layout { return &n.layout }
func (n *Needletail) Port() int              { return port }
func (n *Needletail) HealthCheck() []string {
	return []string{"CMD", "/usr/local/bin/needletail", "healthcheck"}
}
func (n *Needletail) Command(*manifest.Agent) []string { return []string{"serve"} }

// Memory is the same hot-memory format as Hermes: entries joined by "\n§\n".
func (n *Needletail) Memory() engine.MemoryFormat { return engine.Delimited("\n§\n") }

// Reasoning is the levels the core gateway accepts, the same list as Hermes so existing manifests
// stay valid when an agent switches engines.
func (n *Needletail) Reasoning() []string {
	return []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
}

// Image is the pinned image: image_tag, else the version as the tag, with a "v" added only to a
// semver written without one (0.1.0 and v0.1.0 are both :v0.1.0; a tag such as stable stays as
// it is). ValidVersion refuses empty and latest, and build checks it before anything uses the image.
func (n *Needletail) Image(a *manifest.Agent) string {
	tag := a.Engine.ImageTag
	if tag == "" {
		tag = a.Engine.Version
		if semverRe.MatchString(tag) && !strings.HasPrefix(tag, "v") {
			tag = "v" + tag
		}
	}
	return Repository + ":" + tag
}

var (
	semverRe = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)
	tagRe    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// ValidVersion accepts a semver (v optional) or a pinned image tag; never empty or latest.
func (n *Needletail) ValidVersion(v string) error {
	switch {
	case v == "":
		return errors.New("empty: pin a needletail version")
	case v == "latest":
		return errors.New("latest is not pinned: give a version or a tag")
	case semverRe.MatchString(v), tagRe.MatchString(v):
		return nil
	}
	return fmt.Errorf("not a semver or an image tag")
}

// EngineOwnedSkills is empty: the image bundles no skills.
func (n *Needletail) EngineOwnedSkills(func(string) []byte) map[string]bool {
	return map[string]bool{}
}

// LogNoise is nil: no log output is understood yet.
func (n *Needletail) LogNoise() []engine.LogNoise { return nil }

// Runtime is the needletail API.
func (n *Needletail) Runtime() engine.Runtime { return runtime{} }

// NewRuntime is the runtime without an instance.
func NewRuntime() engine.Runtime { return runtime{} }

var (
	_ engine.Engine           = (*Needletail)(nil)
	_ engine.VersionValidator = (*Needletail)(nil)
	_ engine.ScheduleDropper  = (*Needletail)(nil)
)
