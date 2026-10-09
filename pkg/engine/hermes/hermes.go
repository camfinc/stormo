// Package hermes is the Nous Research Hermes Agent engine (https://hermes-agent.nousresearch.com/docs/),
// run from the official image with HERMES_HOME=/opt/data on the task's shared volume. The baseline
// compiled here doubles as a Hermes profile distribution, so `hermes profile install
// dist/<agent>/baseline` works locally.
package hermes

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/env"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/shared"
	"go.yaml.in/yaml/v3"
)

const home = "/opt/data"

// ApprovalTimeoutMinS is the minimum time a person has to approve a dangerous command from Slack.
const ApprovalTimeoutMinS = 900

// Hermes is the engine for one instance (the state dir and generated-file names come from it).
type Hermes struct {
	inst   *instance.Instance
	layout engine.Layout
}

// New builds the engine for an instance.
func New(inst *instance.Instance) *Hermes {
	sd := inst.Names.StateDir
	l := engine.Layout{
		Home:      home,
		SkillsDir: "skills",
		StateRoot: sd, // not "state/": Hermes v0.21 keeps its own runtime state there
		// The image's `hermes` user. Its boot chown is targeted and skips files it did not create, so
		// without this MEMORY.md and the skills would be root-owned and the agent could not learn.
		RuntimeUID: 10000, RuntimeGID: 10000,
		Rules: []engine.SnapshotRule{
			// learning: what the agent taught itself
			{Class: engine.Learning, Glob: "memories/{MEMORY,USER}.md"},
			{Class: engine.Learning, Glob: "skills/.{usage.json,curator_ledger.jsonl,curator_state,bundled_manifest}"},
			{Class: engine.Learning, Glob: "skills/.archive/**"},
			{Class: engine.Learning, Glob: "skills/[!.]*/**"},
			{Class: engine.Learning, Glob: "cron/jobs.json"},
			// state: durable tool state, restored but never harvested into git. Tool databases under
			// the state dir are copied with VACUUM INTO, so a nap never captures a half-written page.
			{Class: engine.State, Glob: sd + "/**/*.{db,sqlite}", SQLite: true},
			{Class: engine.State, Glob: sd + "/**"},
			{Class: engine.State, Glob: "local/**"},
			{Class: engine.State, Glob: "gateway_voice_mode.json"},
			// Gateway pairing approvals and the platform chat directory; without these every
			// redeploy would make the agent refuse the people it already knows.
			{Class: engine.State, Glob: "platforms/pairing/**"},
			{Class: engine.State, Glob: "pairing/**"},
			{Class: engine.State, Glob: "{channel_directory,discord_threads}.json"},
			{Class: engine.State, Glob: "kanban.db", SQLite: true},
			{Class: engine.State, Glob: "cron/executions.db", SQLite: true},
			// raw: conversation history (PII)
			{Class: engine.Raw, Glob: "state.db", SQLite: true},
			{Class: engine.Raw, Glob: "response_store.db", SQLite: true},
			{Class: engine.Raw, Glob: "sessions/**"},
		},
	}
	l.Memory.Memory, l.Memory.User = "memories/MEMORY.md", "memories/USER.md"
	return &Hermes{inst: inst, layout: l}
}

func (h *Hermes) Kind() string           { return "hermes" }
func (h *Hermes) Layout() *engine.Layout { return &h.layout }
func (h *Hermes) Port() int              { return 8642 }
func (h *Hermes) HealthCheck() []string {
	return []string{"CMD-SHELL", "curl -fsS http://localhost:8642/health || exit 1"}
}
func (h *Hermes) Command(*manifest.Agent) []string { return []string{"gateway", "run"} }

func (h *Hermes) Image(a *manifest.Agent) string {
	tag := a.Engine.ImageTag
	if tag == "" {
		tag = "v" + a.Engine.Version
	}
	return "nousresearch/hermes-agent:" + tag
}

// ChannelEnv is the non-secret channel settings as the env vars Hermes reads (tokens come from secrets).
func ChannelEnv(a *manifest.Agent, target manifest.Target) *env.Env {
	e := env.New()
	for _, c := range a.Channels {
		s := c.SlackSettings
		if target == manifest.Local && c.Local != nil {
			s = overlay(s, *c.Local)
		}
		// Platform-scoped open access (Hermes <PLATFORM>_ALLOW_ALL_USERS); never the gateway-wide
		// switch, which would open every other platform too.
		if s.AllowAllUsers != nil && *s.AllowAllUsers {
			e.Set(strings.ToUpper(c.Kind)+"_ALLOW_ALL_USERS", "true")
		}
		if c.Kind == "telegram" {
			// Hermes runs Telegram whenever TELEGRAM_BOT_TOKEN (a secret) is in the env.
			if len(s.AllowedUsers) > 0 {
				e.Set("TELEGRAM_ALLOWED_USERS", strings.Join(s.AllowedUsers, ","))
			}
			if s.HomeChannel != "" {
				e.Set("TELEGRAM_HOME_CHANNEL", s.HomeChannel)
			}
			continue
		}
		if c.Kind != "slack" {
			continue
		}
		if len(s.AllowedUsers) > 0 {
			e.Set("SLACK_ALLOWED_USERS", strings.Join(s.AllowedUsers, ","))
		}
		if s.HomeChannel != "" {
			e.Set("SLACK_HOME_CHANNEL", s.HomeChannel)
		}
		if s.HomeChannelName != "" {
			e.Set("SLACK_HOME_CHANNEL_NAME", s.HomeChannelName)
		}
		bots := s.AllowBots
		if bots == "" {
			bots = "mentions"
		}
		e.Set("SLACK_ALLOW_BOTS", bots)
	}
	return e
}

// overlay is `{...base, ...local}`: a field set locally wins.
func overlay(base, local manifest.SlackSettings) manifest.SlackSettings {
	if local.AllowedUsers != nil {
		base.AllowedUsers = local.AllowedUsers
	}
	if local.AllowAllUsers != nil {
		base.AllowAllUsers = local.AllowAllUsers
	}
	if local.HomeChannel != "" {
		base.HomeChannel = local.HomeChannel
	}
	if local.HomeChannelName != "" {
		base.HomeChannelName = local.HomeChannelName
	}
	if local.AllowBots != "" {
		base.AllowBots = local.AllowBots
	}
	return base
}

func (h *Hermes) Env(a *manifest.Agent, target manifest.Target) *env.Env {
	e := env.New()
	e.Merge(a.Env) // the manifest's non-secret settings; everything below wins over them
	e.Set("HERMES_HOME", home)
	e.Set("API_SERVER_ENABLED", "true")
	e.Set("API_SERVER_HOST", "0.0.0.0")
	e.Set("API_SERVER_PORT", "8642")
	e.Set("SWARM_AGENT", a.ID)
	e.Set("SWARM_UNIT", a.Unit)
	e.Set("SWARM_SHARED_DIR", shared.Root)
	// Hermes file tools refuse writes outside these roots (os.pathsep list, agent/file_safety.py).
	e.Set("HERMES_WRITE_SAFE_ROOT", home+":"+shared.Root)
	for _, s := range a.State {
		e.Set(s.Env, home+"/"+h.layout.StateRoot+"/"+s.Name)
	}
	e.Merge(ChannelEnv(a, target))
	return e
}

// ApplyOverrides sets the keys Stormo owns regardless of what the ported base config says.
func ApplyOverrides(cfg *yaml.Node, a *manifest.Agent, target manifest.Target) error {
	set := func(path string, v any) error { return setPath(cfg, path, v) }
	if err := set("model.default", a.Engine.Model); err != nil {
		return err
	}
	if err := set("model.provider", a.Engine.Provider); err != nil {
		return err
	}
	if target == manifest.Local && a.Engine.Local != nil {
		// Locally the swarm core's gateway serves the model on the ChatGPT subscription; the agent
		// only holds its own core key. Hermes falls back to the AWS model on a 401, a 429 or a dead
		// core, and retries the core every turn.
		aws := yamlMap("provider", a.Engine.Provider, "model", a.Engine.Model)
		if bu := getPath(cfg, "model.base_url"); bu != nil && bu.Value != "" {
			addKey(aws, "base_url", bu)
		}
		list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{aws}}
		if prev := getPath(cfg, "fallback_providers"); prev != nil && prev.Kind == yaml.SequenceNode {
			list.Content = append(list.Content, prev.Content...)
		}
		if err := setNode(cfg, "fallback_providers", list); err != nil {
			return err
		}
		// Custom endpoints speak chat_completions; the core translates to Codex Responses itself.
		deleteKey(getPath(cfg, "model"), "api_mode")
		for _, kv := range [][2]string{{"model.provider", "custom"}, {"model.base_url", manifest.Core.BaseURL}, {"model.key_env", manifest.Core.KeyEnv}, {"model.default", a.Engine.Local.Model}} {
			if err := set(kv[0], kv[1]); err != nil {
				return err
			}
		}
		if err := coreHooks(cfg); err != nil {
			return err
		}
	}

	// On Fargate there is no Docker daemon: the task is the sandbox. The old docker sandbox settings
	// (persistent container, bind mounts, forwarded env) are dead config there, so neutralise them.
	if err := set("terminal.backend", "local"); err != nil {
		return err
	}
	if err := set("terminal.container_persistent", false); err != nil {
		return err
	}
	if err := set("terminal.docker_volumes", "[]"); err != nil {
		return err
	}
	if err := set("terminal.docker_forward_env", []string{}); err != nil {
		return err
	}
	deleteKey(getPath(cfg, "terminal"), "env") // PATH=/usr/bin was for the sandbox image
	pass := []string{}
	if prev := getPath(cfg, "terminal.env_passthrough"); prev != nil && prev.Kind == yaml.SequenceNode {
		for _, n := range prev.Content {
			if !slices.Contains(pass, n.Value) {
				pass = append(pass, n.Value)
			}
		}
	}
	for _, s := range a.State {
		if !slices.Contains(pass, s.Env) {
			pass = append(pass, s.Env)
		}
	}
	if err := set("terminal.env_passthrough", pass); err != nil {
		return err
	}

	// Dangerous-command approvals arrive as Slack buttons a person may not see for minutes; keep a
	// floor for every agent, below the run inactivity limit (agent.gateway_timeout, 1800 s) so a
	// waiting approval can't outlive its run.
	timeout := max(number(getPath(cfg, "approvals.timeout"), 0), ApprovalTimeoutMinS)
	if err := set("approvals.timeout", int(min(timeout, number(getPath(cfg, "agent.gateway_timeout"), 1800)-60))); err != nil {
		return err
	}
	// Task-local ephemeral storage, not NFS. If /opt/data ever moves to EFS this must become "delete".
	if err := set("database.journal_mode", "wal"); err != nil {
		return err
	}
	if err := set("memory.memory_char_limit", a.Learning.MemoryCharLimit); err != nil {
		return err
	}
	return set("memory.user_char_limit", a.Learning.UserCharLimit)
}

// CoreHookName names the outbound hook target that posts an agent's activity to the swarm core.
const CoreHookName = "swarm-core"

// CoreHookEvents are the hooks posted to the core. LLM calls are not among them: their payloads
// carry the whole conversation, and the core's gateway already knows which calls are in flight.
var CoreHookEvents = []string{"on_session_start", "on_session_end", "pre_tool_call", "post_tool_call",
	"pre_approval_request", "post_approval_response"}

// coreHooks adds the core as an outbound hook target (Hermes `hooks.outbound`): session, tool and
// approval events, signed with the agent's core key, so the core knows what the agent is doing
// (docs/core.md §2). Delivery is fire-and-forget on Hermes' side and never blocks a turn. Targets
// the base config already lists are kept; a stale core entry is replaced.
func coreHooks(cfg *yaml.Node) error {
	list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	if prev := getPath(cfg, "hooks.outbound"); prev != nil && prev.Kind == yaml.SequenceNode {
		for _, t := range prev.Content {
			if n := getPath(t, "name"); n == nil || n.Value != CoreHookName {
				list.Content = append(list.Content, t)
			}
		}
	}
	list.Content = append(list.Content, yamlMap("name", CoreHookName, "url", manifest.Core.IngestURL, "events", CoreHookEvents,
		"secret_env", manifest.Core.KeyEnv, "timeout", 5))
	return setNode(cfg, "hooks.outbound", list)
}

func number(n *yaml.Node, def float64) float64 {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return def
	}
	f, err := strconv.ParseFloat(n.Value, 64)
	if err != nil {
		return 0 // Number("abc") is NaN in the TS engine; treat as no value
	}
	return f
}

// readTree adds every file under dir as <prefix>/<rel>, skipping caches and node_modules (a skill
// may ship a package workspace that is installed at runtime, never shipped) and the skipped skill dirs.
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

// Compile returns the baseline files relative to the engine home.
func (h *Hermes) Compile(a *manifest.Agent, ctx engine.CompileContext) (map[string][]byte, error) {
	dir := manifest.AgentDir(ctx.Instance.Root, a.ID)
	files := map[string][]byte{}
	soul, err := os.ReadFile(filepath.Join(dir, "SOUL.md"))
	if err != nil {
		return nil, err
	}
	files["SOUL.md"] = soul

	cfg := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if body, err := os.ReadFile(filepath.Join(dir, "hermes", "config.base.yaml")); err == nil {
		var doc yaml.Node
		if err := yaml.Unmarshal(body, &doc); err != nil {
			return nil, fmt.Errorf("agents/%s/hermes/config.base.yaml: %w", a.ID, err)
		}
		if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
			cfg = doc.Content[0]
		}
	}
	if err := ApplyOverrides(cfg, a, ctx.Target); err != nil {
		return nil, err
	}
	rendered, err := encodeYAML(cfg)
	if err != nil {
		return nil, err
	}
	files["config.yaml"] = append([]byte(fmt.Sprintf("# GENERATED by %s from agents/%s/hermes/config.base.yaml + agent.yaml. Do not edit here.\n", ctx.Instance.Names.Resource, a.ID)), rendered...)

	if err := readTree(filepath.Join(dir, "skills"), "skills", files, ctx.SkipSkills); err != nil {
		return nil, err
	}
	if err := readTree(filepath.Join(dir, "plugins"), "plugins", files, nil); err != nil {
		return nil, err
	}
	// No-agent cron scripts: Hermes runs `script:` only from $HERMES_HOME/scripts/.
	if err := readTree(filepath.Join(dir, "scripts"), "scripts", files, nil); err != nil {
		return nil, err
	}
	for p, body := range ctx.Knowledge {
		files["skills/"+p] = []byte(body)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "hermes", "cron.jobs.json")); err == nil {
		files["cron/jobs.json"] = body
	}
	// Hot-tier seed. Rehydrate merges these with live memory instead of copying them over it.
	files[h.layout.Memory.Memory] = []byte(strings.Join(ctx.Seed.Memory, "\n§\n"))
	files[h.layout.Memory.User] = []byte(strings.Join(ctx.Seed.User, "\n§\n"))

	envReq := []map[string]any{}
	for _, s := range a.Secrets {
		envReq = append(envReq, map[string]any{"name": s, "required": true})
	}
	for _, o := range a.OptionalSecrets {
		envReq = append(envReq, map[string]any{"name": o.Name, "required": false})
	}
	dist := yamlMap("name", a.ID, "version", "0.0.0-swarm", "description", a.Name+": "+a.Role,
		"hermes_requires", ">="+a.Engine.Version, "author", ctx.Instance.Author, "license", "proprietary")
	reqs := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, r := range envReq {
		reqs.Content = append(reqs.Content, yamlMap("name", r["name"], "required", r["required"]))
	}
	addKey(dist, "env_requires", reqs)
	if files["distribution.yaml"], err = encodeYAML(dist); err != nil {
		return nil, err
	}
	example := []string{}
	for _, s := range a.Secrets {
		example = append(example, s+"=")
	}
	for _, o := range a.OptionalSecrets {
		example = append(example, "# optional\n"+o.Name+"=")
	}
	files[".env.EXAMPLE"] = []byte(strings.Join(example, "\n") + "\n")
	return files, nil
}

func (h *Hermes) EngineOwnedSkills(homeDir string) (map[string]bool, error) {
	out := map[string]bool{}
	body, err := os.ReadFile(filepath.Join(homeDir, "skills", ".bundled_manifest"))
	if os.IsNotExist(err) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(string(body), "\n") {
		if name := strings.TrimSpace(strings.SplitN(l, ":", 2)[0]); name != "" {
			out[name] = true
		}
	}
	return out, nil
}

func (h *Hermes) BenchArgv(prompt string) []string {
	return []string{"hermes", "chat", "--oneshot", "--format", "stream-json", "--source", "tool", "-q", prompt}
}

func (h *Hermes) ParseBench(stdout string, exitCode int) engine.BenchRun {
	run := engine.BenchRun{Tools: []string{}, ExitCode: exitCode}
	var text string
	var result map[string]any
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "{") {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch ev["type"] {
		case "tool_use":
			run.Tools = append(run.Tools, fmt.Sprint(ev["name"]))
		case "text":
			if t, ok := ev["text"].(string); ok {
				text += t
			}
		case "result":
			result = ev
		}
	}
	run.Text = text
	if result != nil {
		if t, ok := result["text"].(string); ok {
			run.Text = t
		}
		if c, ok := result["exit_code"].(float64); ok {
			run.ExitCode = int(c)
		}
		if tk, ok := result["tokens"].(map[string]any); ok {
			if total, ok := tk["total"].(float64); ok {
				n := int(total)
				run.Tokens = &n
			}
		}
		if e, ok := result["error"].(string); ok {
			run.Error = e
		}
	}
	return run
}
