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

// listenRe is a channel slack.listen may name: a public or private channel's (or a group DM's) id;
// DMs are always answered.
var listenRe = regexp.MustCompile(`^[CG][A-Z0-9]{8,}$`)

// relayRe is a bot slack.relay_bots may name (its bot id, or its user id); userRe an approver.
var (
	relayRe = regexp.MustCompile(`^[BUW][A-Z0-9]{8,}$`)
	userRe  = regexp.MustCompile(`^[UW][A-Z0-9]{8,}$`)
)

// agentConfig is engine/needletail/config.yaml.
type agentConfig struct {
	BackgroundLearning bool `yaml:"background_learning"`
	SkillMaintenance   bool `yaml:"skill_maintenance"`
	Reflection         bool `yaml:"reflection"`
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
		// Listen are the channels the agent listens to (needletail.yaml slack.listen): channel
		// ids, or "*" alone for every channel the bot is in. Every other message there reaches
		// the agent's on_message handlers only (docs/needletail.md §4.9).
		Listen []string `yaml:"listen"`
		// RelayBots are allowed bots whose messages still mark the conversation (needletail.yaml
		// slack.relay_bots): they relay other people's words.
		RelayBots []string `yaml:"relay_bots"`
	} `yaml:"slack"`
	// Approvals are who decides what a tainted run or a risky command needs a person for, how
	// long a request waits, the caps and extra risky-command patterns (needletail.yaml approvals).
	Approvals *wire.ApprovalsConfig `yaml:"approvals"`
	Schedules struct {
		// Turns is how many scheduled runs may hold a turn at once (needletail.yaml
		// schedules.turns; the runtime's 1 when 0).
		Turns int `yaml:"turns"`
	} `yaml:"schedules"`
	Limits struct {
		// BackgroundLifetime is how long a background process (terminal background: true) may
		// run, in seconds (needletail.yaml limits.background_lifetime; the runtime's 2 h when 0).
		BackgroundLifetime int `yaml:"background_lifetime"`
		// ChatTurns is how many chat turns may run at once (needletail.yaml limits.chat_turns;
		// the runtime's 2 when 0).
		ChatTurns int `yaml:"chat_turns"`
	} `yaml:"limits"`
	// Files are what the runtime does with the files people send (needletail.yaml files):
	// retention_days (1 to 365; 14 when unset) and vision (auto, on/true, off/false).
	Files *wire.FilesConfig `yaml:"files"`
	// STT transcribes voice notes (needletail.yaml stt): enabled, provider (elevenlabs), language
	// (an ISO 639 code), model. Its key is ELEVENLABS_API_KEY, which agent.yaml must declare.
	STT *wire.STTConfig `yaml:"stt"`
	// Web is the agent's web search (needletail.yaml web): search {provider (brave, tavily),
	// max_results (1 to 20; 5 when unset)}. Its key (BRAVE_SEARCH_API_KEY, TAVILY_API_KEY) must be
	// among agent.yaml's secrets.
	Web    *wire.WebConfig `yaml:"web"`
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

// maxRetentionDays is the longest files.retention_days.
const maxRetentionDays = 365

// The speech-to-text provider, its key and the shapes of stt.language and stt.model.
const (
	sttProvider = "elevenlabs"
	sttKeyEnv   = "ELEVENLABS_API_KEY"
)

var (
	languageRe = regexp.MustCompile(`^[a-z]{2,3}$`)
	sttModelRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)
)

// checkSTTKey: speech-to-text needs its key among the agent's secrets (agent.yaml secrets or
// optional_secrets), or the runtime would have none.
func checkSTTKey(a *manifest.Agent, ac *agentConfig) error {
	if ac.STT == nil || !ac.STT.Enabled {
		return nil
	}
	if slices.Contains(a.Secrets, sttKeyEnv) {
		return nil
	}
	for _, o := range a.OptionalSecrets {
		if o.Name == sttKeyEnv {
			return nil
		}
	}
	return fmt.Errorf("%s: stt is enabled but agent.yaml does not declare %s among its secrets", ConfigFile, sttKeyEnv)
}

// checkSearchKey: a web search provider needs its key among the agent's secrets (agent.yaml
// secrets or optional_secrets), or the runtime would have none.
func checkSearchKey(a *manifest.Agent, ac *agentConfig) error {
	if ac.Web == nil || ac.Web.Search == nil {
		return nil
	}
	key := wire.SearchKeyEnv(ac.Web.Search.Provider)
	if slices.Contains(a.Secrets, key) || slices.ContainsFunc(a.OptionalSecrets, func(o manifest.OptionalSecret) bool { return o.Name == key }) {
		return nil
	}
	return fmt.Errorf("%s: web.search uses %s but agent.yaml does not declare %s among its secrets", ConfigFile, ac.Web.Search.Provider, key)
}

// maxScheduleTurns is the most turns scheduled runs may hold at once.
const maxScheduleTurns = 4

// maxLuauTimeout is the longest luau.timeout (a tool header's own longest, luau.MaxTimeout).
const maxLuauTimeout = 600

// maxChatTurns is the most chat turns limits.chat_turns may allow at once.
const maxChatTurns = 16

// maxBackgroundLifetime is the longest limits.background_lifetime (the runtime's
// tools.MaxLifetime), in seconds.
const maxBackgroundLifetime = 24 * 3600

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
			return nil, fmt.Errorf("%s: luau grant %q is not a capability (fs.read:home, fs.write:home[/glob], fs.*:shared, http:<host>, core:<tool>, message:<target>, memory:read|write, kv:read|write, followup:read|write)", ConfigFile, g)
		}
	}
	for _, s := range c.Luau.HTTPSecrets {
		if s.Env == "NEEDLETAIL_LEARNING_KEY" {
			return nil, fmt.Errorf("%s: owner learning key is reserved", ConfigFile)
		}
		if s.Host == "" || s.Header == "" || !envNameRe.MatchString(s.Env) {
			return nil, fmt.Errorf("%s: http_secrets entries need host, header and an env var name", ConfigFile)
		}
	}
	for _, ch := range c.Slack.Listen {
		if ch == "*" && len(c.Slack.Listen) > 1 || ch != "*" && !listenRe.MatchString(ch) {
			return nil, fmt.Errorf("%s: slack.listen is [\"*\"] (every channel the bot is in) or a list of channel ids (C…, G…), not %q", ConfigFile, ch)
		}
	}
	for _, b := range c.Slack.RelayBots {
		if !relayRe.MatchString(b) {
			return nil, fmt.Errorf("%s: slack.relay_bots names bots by id (B…, or their user id U…), not %q", ConfigFile, b)
		}
	}
	if a := c.Approvals; a != nil {
		for _, u := range a.Approvers {
			if !userRe.MatchString(u) {
				return nil, fmt.Errorf("%s: approvals.approvers are Slack user ids (U…, W…) within allowed_users, not %q", ConfigFile, u)
			}
		}
		switch {
		case a.Timeout < 0 || a.Timeout > 24*3600:
			return nil, fmt.Errorf("%s: approvals.timeout %d must be seconds, 60 to %d (0 or unset: 1800)", ConfigFile, a.Timeout, 24*3600)
		case a.Timeout > 0 && a.Timeout < 60:
			return nil, fmt.Errorf("%s: approvals.timeout %d must be seconds, 60 to %d (0 or unset: 1800)", ConfigFile, a.Timeout, 24*3600)
		case a.DeliveryExpiryDays < 0 || a.DeliveryExpiryDays > 30:
			return nil, fmt.Errorf("%s: approvals.delivery_expiry_days %d must be 1 to 30 (0 or unset: 3)", ConfigFile, a.DeliveryExpiryDays)
		case a.PerConversation < 0 || a.PerConversation > 20:
			return nil, fmt.Errorf("%s: approvals.per_conversation %d must be 1 to 20 (0 or unset: 3)", ConfigFile, a.PerConversation)
		case a.PerDay < 0 || a.PerDay > 500:
			return nil, fmt.Errorf("%s: approvals.per_day %d must be 1 to 500 (0 or unset: 20)", ConfigFile, a.PerDay)
		}
		for _, r := range a.Commands {
			if _, err := regexp.Compile(`(?i)` + r.Pattern); err != nil || strings.TrimSpace(r.Pattern) == "" {
				return nil, fmt.Errorf("%s: approvals.commands pattern %q is not a regular expression", ConfigFile, r.Pattern)
			}
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
	if f := c.Files; f != nil && (f.RetentionDays < 0 || f.RetentionDays > maxRetentionDays) {
		return nil, fmt.Errorf("%s: files.retention_days %d must be 1 to %d (0 or unset: 14)", ConfigFile, f.RetentionDays, maxRetentionDays)
	}
	if s := c.STT; s != nil {
		switch {
		case s.Provider != "" && s.Provider != sttProvider:
			return nil, fmt.Errorf("%s: stt.provider %q: the only one is %s", ConfigFile, s.Provider, sttProvider)
		case s.Language != "" && !languageRe.MatchString(s.Language):
			return nil, fmt.Errorf("%s: stt.language %q is an ISO 639-1 or 639-3 code (es, en, spa)", ConfigFile, s.Language)
		case s.Model != "" && !sttModelRe.MatchString(s.Model):
			return nil, fmt.Errorf("%s: stt.model %q is not a model id", ConfigFile, s.Model)
		}
	}
	if l := c.Limits.BackgroundLifetime; l < 0 || l > maxBackgroundLifetime {
		return nil, fmt.Errorf("%s: limits.background_lifetime %d must be seconds, 1 to %d (0 or unset: the runtime's 7200, 2 h)", ConfigFile, l, maxBackgroundLifetime)
	}
	if w := c.Web; w != nil && w.Search != nil {
		if wire.SearchKeyEnv(w.Search.Provider) == "" {
			return nil, fmt.Errorf("%s: web.search.provider %q: brave or tavily", ConfigFile, w.Search.Provider)
		}
		if w.Search.MaxResults < 0 || w.Search.MaxResults > wire.MaxSearchResults {
			return nil, fmt.Errorf("%s: web.search.max_results %d must be 1 to %d (0 or unset: 5)", ConfigFile, w.Search.MaxResults, wire.MaxSearchResults)
		}
	}
	if c.Limits.ChatTurns < 0 || c.Limits.ChatTurns > maxChatTurns {
		return nil, fmt.Errorf("%s: limits.chat_turns %d must be 1 to %d (0 or unset: the runtime's 2)", ConfigFile, c.Limits.ChatTurns, maxChatTurns)
	}
	if c.Luau.Timeout < 0 || c.Luau.Timeout > maxLuauTimeout {
		return nil, fmt.Errorf("%s: luau.timeout %d must be seconds, 1 to %d (0 or unset: the runtime's 30)", ConfigFile, c.Luau.Timeout, maxLuauTimeout)
	}
	return c, nil
}

// scrubConfig is the instance's own scrub rules for the runtime, nil when it has none.
func scrubConfig(inst *instance.Instance) *wire.ScrubConfig {
	if len(inst.ScrubTokenPrefixes) == 0 && len(inst.ScrubRules) == 0 {
		return nil
	}
	c := &wire.ScrubConfig{TokenPrefixes: inst.ScrubTokenPrefixes}
	for _, r := range inst.ScrubRules {
		c.Rules = append(c.Rules, wire.ScrubRule{Kind: r.Kind, Pattern: r.Pattern, Require: r.Require})
	}
	return c
}

// DefaultWriteGrant is the home write every agent's Luau is granted: its own conversation's work
// directory only (work/<conversation>/: the runtime refuses another conversation's). Before
// 2026-10-28 it was the whole home (fs.write:home); an agent whose tools or .luau job scripts write
// elsewhere grants that directory in engine/needletail/config.yaml (fs.write:home/<dir>/**).
const DefaultWriteGrant = "fs.write:home/work/**"

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
		Limits: wire.Limits{Turns: a.Limits.Turns, CommandTimeout: a.Limits.CommandTimeout, ScriptTimeout: a.Limits.ScriptTimeout, LuauTimeout: ac.Luau.Timeout,
			BackgroundLifetime: ac.Limits.BackgroundLifetime, ChatTurns: ac.Limits.ChatTurns},
		Memory: wire.MemoryConfig{MemoryCharLimit: a.Learning.MemoryCharLimit, UserCharLimit: a.Learning.UserCharLimit, Recall: ac.Memory.Recall}}
	c.Reflection = ac.Reflection
	c.BackgroundLearning = ac.BackgroundLearning
	c.SkillMaintenance = ac.SkillMaintenance
	c.Model.Primary = cloud
	grants := []string{"fs.read:home", DefaultWriteGrant}
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
	if ac.Slack.Broadcast || len(ac.Slack.Listen) > 0 || len(ac.Slack.RelayBots) > 0 {
		c.Slack = &wire.SlackConfig{Broadcast: ac.Slack.Broadcast, Listen: ac.Slack.Listen, RelayBots: ac.Slack.RelayBots}
	}
	c.Approvals = ac.Approvals
	c.Files, c.STT = ac.Files, ac.STT
	if ac.Web != nil && ac.Web.Search != nil {
		c.Web = ac.Web
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

// scheduleProviders are the API connections a job may name (needletail.yaml model.providers): the
// agent's own and each one agent.yaml's schedules name, by their instance names; none when no
// schedule names a provider (the file stays as an older image reads it).
func scheduleProviders(a *manifest.Agent, inst *instance.Instance) ([]wire.Provider, error) {
	named := []string{}
	for _, s := range a.Schedules {
		if s.Provider != "" && !slices.Contains(named, s.Provider) {
			named = append(named, s.Provider)
		}
	}
	if len(named) == 0 {
		return nil, nil
	}
	if !slices.Contains(named, a.Engine.Provider) {
		named = append([]string{a.Engine.Provider}, named...)
	}
	out := []wire.Provider{}
	for _, n := range named {
		c, ok := inst.Connection(n)
		if !ok || !c.API() {
			return nil, fmt.Errorf("a schedule's provider %q is not an API connection of this instance (stormo.yaml connections:)", n)
		}
		out = append(out, wire.Provider{Name: n, BaseURL: c.BaseURL, KeyEnv: c.Key})
	}
	return out, nil
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
	if err := checkSTTKey(a, ac); err != nil {
		return nil, fmt.Errorf("agents/%s: %w", a.ID, err)
	}
	if err := checkSearchKey(a, ac); err != nil {
		return nil, fmt.Errorf("agents/%s: %w", a.ID, err)
	}
	rendered := renderConfig(a, conn, ctx.Target, manifest.CoreForPlacement(ctx.Instance, ctx.Target, ctx.Placement), ac)
	if rendered.Model.Providers, err = scheduleProviders(a, ctx.Instance); err != nil {
		return nil, fmt.Errorf("agents/%s: %w", a.ID, err)
	}
	rendered.Skills.Generated = generatedSkills(ctx.Knowledge)
	rendered.StateRoot = n.layout.StateRoot
	rendered.Scrub = scrubConfig(ctx.Instance)
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
