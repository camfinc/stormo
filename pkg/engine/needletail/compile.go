package needletail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"go.yaml.in/yaml/v3"
)

var envNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// agentConfig is engine/needletail/config.yaml.
type agentConfig struct {
	// Timezone is the agent's wall clock (an IANA zone, as Hermes' config.yaml timezone).
	Timezone string `yaml:"timezone"`
	Luau     struct {
		Grants      []string          `yaml:"grants"`
		HTTPSecrets []wire.HTTPSecret `yaml:"http_secrets"`
		// Timeout bounds a Luau tool the model calls, in seconds (needletail.yaml
		// limits.luau_timeout; the runtime's default when 0). Schedules keep script_timeout.
		Timeout int `yaml:"timeout"`
	} `yaml:"luau"`
	Slack struct {
		// Broadcast lets send_message ping @channel, @here, @everyone and user groups when a
		// call asks for it (needletail.yaml slack.broadcast).
		Broadcast bool `yaml:"broadcast"`
	} `yaml:"slack"`
	Schedules struct {
		// Turns is how many scheduled runs may hold a turn at once (needletail.yaml
		// schedules.turns; the runtime's 1 when 0).
		Turns int `yaml:"turns"`
	} `yaml:"schedules"`
	Memory struct {
		// Recall is the per-turn recall from the memory archive (needletail.yaml memory.recall;
		// omitted when unset: on for user turns, off for scheduled runs).
		Recall *wire.RecallConfig `yaml:"recall"`
	} `yaml:"memory"`
}

// Bounds of memory.recall.
const (
	maxRecallRecords = 10
	minRecallBudget  = 200
	maxRecallBudget  = 4000
)

// maxScheduleTurns is the most turns scheduled runs may hold at once.
const maxScheduleTurns = 4

// maxLuauTimeout is the longest luau.timeout (a tool header's own longest, luau.MaxTimeout).
const maxLuauTimeout = 600

func readAgentConfig(dir string) (*agentConfig, error) {
	c := &agentConfig{}
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil || c.Timezone == "Local" {
			return nil, fmt.Errorf("%s: timezone %q is not an IANA zone (America/New_York, Europe/Lisbon, UTC)", ConfigFile, c.Timezone)
		}
	}
	for _, g := range c.Luau.Grants {
		if !wire.Capability.MatchString(g) {
			return nil, fmt.Errorf("%s: luau grant %q is not a capability (fs.read:home, fs.write:home[/glob], fs.*:shared, http:<host>, core:<tool>)", ConfigFile, g)
		}
	}
	for _, s := range c.Luau.HTTPSecrets {
		if s.Host == "" || s.Header == "" || !envNameRe.MatchString(s.Env) {
			return nil, fmt.Errorf("%s: http_secrets entries need host, header and an env var name", ConfigFile)
		}
	}
	if c.Schedules.Turns < 0 || c.Schedules.Turns > maxScheduleTurns {
		return nil, fmt.Errorf("%s: schedules.turns %d must be 1 to %d (0 or unset: the runtime's 1)", ConfigFile, c.Schedules.Turns, maxScheduleTurns)
	}
	if r := c.Memory.Recall; r != nil {
		if r.Max < 0 || r.Max > maxRecallRecords {
			return nil, fmt.Errorf("%s: memory.recall.max %d must be 1 to %d records (0 or unset: the runtime's 5)", ConfigFile, r.Max, maxRecallRecords)
		}
		if r.Budget != 0 && (r.Budget < minRecallBudget || r.Budget > maxRecallBudget) {
			return nil, fmt.Errorf("%s: memory.recall.budget %d must be %d to %d UTF-16 units (0 or unset: the runtime's 1200)", ConfigFile, r.Budget, minRecallBudget, maxRecallBudget)
		}
	}
	if c.Luau.Timeout < 0 || c.Luau.Timeout > maxLuauTimeout {
		return nil, fmt.Errorf("%s: luau.timeout %d must be seconds, 1 to %d (0 or unset: the runtime's 30)", ConfigFile, c.Luau.Timeout, maxLuauTimeout)
	}
	return c, nil
}

// onCore: a local run routed through the swarm core (engine.local), as for Hermes.
func onCore(a *manifest.Agent, target manifest.Target) bool {
	return target == manifest.Local && a.Engine.Local != nil
}

// renderConfig is needletail.yaml. The model connection resolves as Hermes' does: on the core, the
// core's gateway first and the agent's connection as the fallback; elsewhere the connection alone.
// core is the instance's core as this placement reaches it (manifest.CoreForPlacement): its own
// port or URL, with hooks and MCP through the sidecar's relay.
func renderConfig(a *manifest.Agent, conn instance.Connection, target manifest.Target, core manifest.CoreEndpoints, ac *agentConfig) wire.Config {
	cloud := wire.Endpoint{BaseURL: conn.BaseURL, KeyEnv: conn.Key, Model: a.Engine.Model}
	c := wire.Config{Version: wire.ConfigVersion, Agent: a.ID, Reasoning: a.Limits.Reasoning,
		Limits: wire.Limits{Turns: a.Limits.Turns, CommandTimeout: a.Limits.CommandTimeout, ScriptTimeout: a.Limits.ScriptTimeout, LuauTimeout: ac.Luau.Timeout},
		Memory: wire.MemoryConfig{MemoryCharLimit: a.Learning.MemoryCharLimit, UserCharLimit: a.Learning.UserCharLimit, Recall: ac.Memory.Recall}}
	c.Model.Primary = cloud
	grants := []string{"fs.read:home", "fs.write:home"}
	if onCore(a, target) && !core.NoGateway {
		c.Model.Primary = wire.Endpoint{BaseURL: core.BaseURL, KeyEnv: core.KeyEnv, Model: a.Engine.Local.Model}
		c.Model.Fallbacks = []wire.Endpoint{cloud}
	}
	if onCore(a, target) || core.Remote && a.Engine.Local != nil {
		c.Core = &wire.CoreConfig{MCPURL: core.MCPURL, IngestURL: core.IngestURL, KeyEnv: core.KeyEnv}
		grants = append(grants, "core:msg_*")
	}
	for _, g := range ac.Luau.Grants {
		if !slices.Contains(grants, g) {
			grants = append(grants, g)
		}
	}
	c.Luau = wire.LuauConfig{Grants: grants, HTTPSecrets: ac.Luau.HTTPSecrets}
	c.Timezone = ac.Timezone
	if ac.Slack.Broadcast {
		c.Slack = &wire.SlackConfig{Broadcast: true}
	}
	c.Schedules.Turns = ac.Schedules.Turns
	for _, s := range a.Schedules {
		c.Schedules.Repo = append(c.Schedules.Repo, s.ID)
	}
	for _, ch := range a.Channels {
		if ch.Kind == "slack" { // the only gateway in the MVP
			c.Channels = append(c.Channels, ch.Kind)
		}
	}
	return c
}

// readTree adds every file under dir as <prefix>/<rel>, skipping caches, node_modules and the
// skipped skill dirs (the same walk as Hermes').
func readTree(dir, prefix string, out map[string][]byte, skip []string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if strings.Contains(rel, "__pycache__") || strings.HasSuffix(rel, ".pyc") || strings.HasSuffix(rel, ".DS_Store") || slices.Contains(strings.Split(rel, "/"), "node_modules") {
			return nil
		}
		for _, s := range skip {
			if strings.HasPrefix(rel, s+"/") {
				return nil
			}
		}
		if !d.Type().IsRegular() {
			if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
				return nil
			}
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out[prefix+"/"+rel] = body
		return nil
	})
}

// generatedSkills is the skill dirs the build generates (the knowledge files' skills, keyed by
// their path under skills/): each dir holding a SKILL.md among them, else a file's top directory.
// The runtime refuses to change them (needletail.yaml skills.generated).
func generatedSkills(knowledge map[string]string) []string {
	dirs := map[string]bool{}
	for p := range knowledge {
		if strings.HasSuffix(p, "/SKILL.md") {
			dirs[strings.TrimSuffix(p, "/SKILL.md")] = true
		}
	}
	for p := range knowledge {
		covered := false
		for d := range dirs {
			if strings.HasPrefix(p, d+"/") {
				covered = true
			}
		}
		if top, _, ok := strings.Cut(p, "/"); !covered && ok {
			dirs[top] = true
		}
	}
	out := make([]string, 0, len(dirs))
	for d := range dirs {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// Compile returns the baseline files relative to the engine home. Skills stay in Stormo's form
// (the runtime resolves ${SKILL_DIR} itself). A new engine has no format 0, so there is no legacy
// schedules file to reconcile: schedules come from agent.yaml only.
func (n *Needletail) Compile(a *manifest.Agent, ctx engine.CompileContext) (map[string][]byte, error) {
	if r := a.Limits.Reasoning; r != "" && !slices.Contains(n.Reasoning(), r) {
		return nil, fmt.Errorf("agents/%s: limits.reasoning %q is not a needletail level (%s)", a.ID, r, strings.Join(n.Reasoning(), ", "))
	}
	dir := manifest.AgentDir(ctx.Instance.Root, a.ID)
	files := map[string][]byte{}
	soul, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		return nil, err
	}
	files["SOUL.md"] = soul
	conn, ok := ctx.Instance.Connection(a.Engine.Provider)
	if !ok {
		return nil, fmt.Errorf("agents/%s: model.provider %q is not a connection of this instance", a.ID, a.Engine.Provider)
	}
	ac, err := readAgentConfig(dir)
	if err != nil {
		return nil, fmt.Errorf("agents/%s: %w", a.ID, err)
	}
	rendered := renderConfig(a, conn, ctx.Target, manifest.CoreForPlacement(ctx.Instance, ctx.Target, ctx.Placement), ac)
	rendered.Skills.Generated = generatedSkills(ctx.Knowledge)
	rendered.StateRoot = n.layout.StateRoot
	cfg, err := yaml.Marshal(rendered)
	if err != nil {
		return nil, err
	}
	files["needletail.yaml"] = append([]byte(fmt.Sprintf("# GENERATED by %s from agents/%s/agent.yaml and %s. Do not edit here.\n", ctx.Instance.Names.Resource, a.ID, ConfigFile)), cfg...)

	if err := readTree(filepath.Join(dir, "skills"), "skills", files, ctx.SkipSkills); err != nil {
		return nil, err
	}
	if err := readTree(filepath.Join(dir, "scripts"), "scripts", files, nil); err != nil {
		return nil, err
	}
	for p, body := range ctx.Knowledge {
		files["skills/"+p] = []byte(body)
	}
	if a.Schedules != nil {
		if files[n.layout.Schedules], err = renderJobs(a.Schedules); err != nil {
			return nil, err
		}
	}
	// Hot-tier seed. Rehydrate merges these with live memory instead of copying them over it.
	files[n.layout.Memory.Memory] = []byte(n.Memory().File(ctx.Seed.Memory))
	files[n.layout.Memory.User] = []byte(n.Memory().File(ctx.Seed.User))
	return files, nil
}
