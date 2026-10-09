// Package manifest loads and validates an instance's units and agents (units/<u>/unit.yaml,
// agents/<id>/agent.yaml), and applies optional secrets.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/env"
	"go.yaml.in/yaml/v3"
)

// Target is where an agent runs; it picks channel overrides and the secrets overlay.
type Target string

const (
	AWS   Target = "aws"
	Local Target = "local"
)

// Scope of a learning or knowledge entry.
type Scope string

const (
	ScopeAgent Scope = "agent"
	ScopeUnit  Scope = "unit"
	ScopeGroup Scope = "group"
)

// SlackSettings are the per-target, non-secret channel settings.
type SlackSettings struct {
	// Who may talk to the bot (others must be paired): Slack Member IDs, or Telegram numeric user ids.
	AllowedUsers []string `json:"allowed_users,omitempty"`
	// Anyone on the platform may talk to the bot. Overrides allowed_users.
	AllowAllUsers *bool `json:"allow_all_users,omitempty"`
	// Channel / chat id for cron and scheduled deliveries.
	HomeChannel     string `json:"home_channel,omitempty"`
	HomeChannelName string `json:"home_channel_name,omitempty"`
	// Slack only. Messages from other bots: none | mentions | all.
	AllowBots string `json:"allow_bots,omitempty"`
}

// Channel is one messaging platform an agent is reachable on.
type Channel struct {
	Kind string `json:"kind"`
	SlackSettings
	// Slack only. This agent's app registers the engine's native slash commands; at most one agent
	// per workspace sets it (command names are workspace-wide).
	SlashCommands *bool `json:"slash_commands,omitempty"`
	// Overrides used when running locally (e.g. a dev workspace with different ids).
	Local *SlackSettings `json:"local,omitempty"`
}

// ChannelSecrets are the secret names each channel kind needs declared.
var ChannelSecrets = map[string][]string{
	"slack":    {"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"},
	"telegram": {"TELEGRAM_BOT_TOKEN"},
}

// Core is the swarm core's model gateway as local agent containers see it. Each agent
// authenticates with its own key, a local-only secret (never pushed to AWS).
var Core = struct{ KeyEnv, BaseURL string }{
	KeyEnv: "SWARM_CORE_KEY",
	// The core runs on the host (it drives the local compose projects), so containers reach it here.
	BaseURL: "http://host.docker.internal:18600/v1",
}

// OptionalSecret is a secret the agent can run without; when absent, what it gates is left out.
type OptionalSecret struct {
	Name    string   `json:"name"`
	Skills  []string `json:"skills"`
	Actions []string `json:"actions"`
}

// Persona is the agent's look: `path` inside this instance, or inside sibling repo `repo`.
type Persona struct {
	Repo string `json:"repo,omitempty"`
	Path string `json:"path"`
}

// EngineLocal routes a local run through the swarm core's model gateway, keeping the AWS model as
// its fallback. Never compiled into the AWS baseline.
type EngineLocal struct {
	Via   string `json:"via"`
	Model string `json:"model"`
}

type EngineSpec struct {
	Kind     string       `json:"kind"`
	Version  string       `json:"version"`
	ImageTag string       `json:"image_tag,omitempty"`
	Model    string       `json:"model"`
	Provider string       `json:"provider"`
	Local    *EngineLocal `json:"local,omitempty"`
}

type State struct {
	Name string `json:"name"`
	Env  string `json:"env"`
}

type Learning struct {
	MemoryCharLimit    int     `json:"memory_char_limit"`
	UserCharLimit      int     `json:"user_char_limit"`
	SeedFill           float64 `json:"seed_fill"`
	NapIntervalSeconds int     `json:"nap_interval_seconds"`
}

type Deploy struct {
	Launch              string `json:"launch"`
	Registry            string `json:"registry"`
	CPU                 int    `json:"cpu"`
	Memory              int    `json:"memory"`
	EphemeralStorageGiB int    `json:"ephemeral_storage_gib"`
	SecretID            string `json:"secret_id"`
}

// Agent is a validated agents/<id>/agent.yaml with defaults filled.
type Agent struct {
	ID              string           `json:"id"`
	Name            string           `json:"name"`
	Unit            string           `json:"unit"`
	Role            string           `json:"role"`
	Persona         *Persona         `json:"persona,omitempty"`
	Engine          EngineSpec       `json:"engine"`
	Channels        []Channel        `json:"channels"`
	Secrets         []string         `json:"secrets"`
	OptionalSecrets []OptionalSecret `json:"optional_secrets"`
	Actions         []string         `json:"actions"`
	// Non-secret settings exported to the agent's container as-is; never a credential.
	Env      *env.Env `json:"env"`
	State    []State  `json:"state"`
	Learning Learning `json:"learning"`
	Deploy   Deploy   `json:"deploy"`
}

// Unit is units/<id>/unit.yaml.
type Unit struct {
	ID          string `json:"id" yaml:"id"`
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description" yaml:"description"`
}

// Error is a manifest validation failure.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func req(cond bool, format string, a ...any) error {
	if cond {
		return nil
	}
	return &Error{fmt.Sprintf(format, a...)}
}

var (
	slugRe    = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	envNameRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	credRe    = regexp.MustCompile(`(TOKEN|KEY|SECRET|PASSWORD)$`)
	memberRe  = regexp.MustCompile(`^[UW][A-Z0-9]{6,}$`)
	numericRe = regexp.MustCompile(`^\d+$`)
)

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// UnitIDs lists units/<id>/ directories that hold a unit.yaml, sorted.
func UnitIDs(root string) []string { return idsWith(filepath.Join(root, "units"), "unit.yaml") }

// AgentIDs lists agents/<id>/ directories that hold an agent.yaml, sorted.
func AgentIDs(root string) []string { return idsWith(filepath.Join(root, "agents"), "agent.yaml") }

func idsWith(dir, file string) []string {
	entries, _ := os.ReadDir(dir)
	out := []string{}
	for _, e := range entries {
		if exists(filepath.Join(dir, e.Name(), file)) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// AgentDir is agents/<id> of the instance at root.
func AgentDir(root, id string) string { return filepath.Join(root, "agents", id) }

// LoadUnit reads units/<id>/unit.yaml.
func LoadUnit(root, id string) (*Unit, error) {
	body, err := os.ReadFile(filepath.Join(root, "units", id, "unit.yaml"))
	if err != nil {
		return nil, err
	}
	var u Unit
	if err := yaml.Unmarshal(body, &u); err != nil {
		return nil, fmt.Errorf("units/%s/unit.yaml: %w", id, err)
	}
	if err := req(u.ID == id, `units/%s/unit.yaml: id must be "%s"`, id, id); err != nil {
		return nil, err
	}
	return &u, nil
}

// LocalOnlySecrets are secret names an agent needs only when running locally.
func LocalOnlySecrets(engine EngineSpec) []string {
	if engine.Local != nil && engine.Local.Via == "core" {
		return []string{Core.KeyEnv}
	}
	return nil
}

// raw mirrors agent.yaml loosely so validation can report what is wrong instead of a decode error.
type raw struct {
	ID      any `yaml:"id"`
	Name    any `yaml:"name"`
	Unit    any `yaml:"unit"`
	Role    any `yaml:"role"`
	Persona *struct {
		Repo string `yaml:"repo"`
		Path string `yaml:"path"`
	} `yaml:"persona"`
	Engine *struct {
		Kind     any `yaml:"kind"`
		Version  any `yaml:"version"`
		ImageTag any `yaml:"image_tag"`
		Model    any `yaml:"model"`
		Provider any `yaml:"provider"`
		Local    *struct {
			Via   any `yaml:"via"`
			Model any `yaml:"model"`
		} `yaml:"local"`
	} `yaml:"engine"`
	Channels        []map[string]any `yaml:"channels"`
	Secrets         []any            `yaml:"secrets"`
	OptionalSecrets []map[string]any `yaml:"optional_secrets"`
	Actions         []any            `yaml:"actions"`
	Env             yaml.Node        `yaml:"env"`
	State           []State          `yaml:"state"`
	Learning        map[string]any   `yaml:"learning"`
	Deploy          map[string]any   `yaml:"deploy"`
}

func str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}

func strs(v []any) []string {
	out := make([]string, 0, len(v))
	for _, x := range v {
		out = append(out, str(x))
	}
	return out
}

func anyStrs(v any) []string {
	if l, ok := v.([]any); ok {
		return strs(l)
	}
	return nil
}

func optBool(v any, ok *bool) *bool {
	if v == nil {
		return nil
	}
	b, isBool := v.(bool)
	if !isBool {
		*ok = false
		return nil
	}
	return &b
}

func settings(m map[string]any, okBools *bool) SlackSettings {
	return SlackSettings{
		AllowedUsers:    anyStrs(m["allowed_users"]),
		AllowAllUsers:   optBool(m["allow_all_users"], okBools),
		HomeChannel:     str(m["home_channel"]),
		HomeChannelName: str(m["home_channel_name"]),
		AllowBots:       str(m["allow_bots"]),
	}
}

// Load parses and validates agents/<id>/agent.yaml of the instance at root, with secretPrefix
// (stormo.yaml names.secret) as the default Secrets Manager id prefix.
func Load(root, id, secretPrefix string) (*Agent, error) {
	path := filepath.Join(AgentDir(root, id), "agent.yaml")
	if err := req(exists(path), "no manifest at %s", path); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(root, id, secretPrefix, body)
}

// Parse validates body as agents/<id>/agent.yaml of the instance at root, whatever the file on
// disk holds: what a proposed edit would load as (pkg/config validates with it before writing).
func Parse(root, id, secretPrefix string, body []byte) (*Agent, error) {
	where := fmt.Sprintf("agents/%s/agent.yaml", id)
	var m raw
	if err := yaml.Unmarshal(body, &m); err != nil {
		return nil, &Error{fmt.Sprintf("%s: %v", where, err)}
	}
	mid, unit := str(m.ID), str(m.Unit)
	checks := []error{
		req(mid == id, `%s: id must equal directory name "%s"`, where, id),
		req(slugRe.MatchString(mid), "%s: id must be a lowercase slug", where),
		req(str(m.Name) != "", "%s: name is required", where),
		req(unit != "" && unit != "group", `%s: unit is required and cannot be "group"`, where),
	}
	for _, e := range checks {
		if e != nil {
			return nil, e
		}
	}
	if err := req(slices.Contains(UnitIDs(root), unit), `%s: unknown unit "%s" (see units/)`, where, unit); err != nil {
		return nil, err
	}
	if err := req(m.Engine != nil && str(m.Engine.Kind) != "" && str(m.Engine.Version) != "" && str(m.Engine.Model) != "", "%s: engine.kind/version/model are required", where); err != nil {
		return nil, err
	}
	if err := req(exists(filepath.Join(AgentDir(root, id), "SOUL.md")), "%s: SOUL.md is missing", where); err != nil {
		return nil, err
	}
	engine := EngineSpec{Kind: str(m.Engine.Kind), Version: str(m.Engine.Version), ImageTag: str(m.Engine.ImageTag), Model: str(m.Engine.Model), Provider: or(str(m.Engine.Provider), "openrouter")}
	if l := m.Engine.Local; l != nil {
		if err := req(str(l.Via) == "core", `%s: engine.local.via must be "core"`, where); err != nil {
			return nil, err
		}
		if err := req(str(l.Model) != "", "%s: engine.local.model is required", where); err != nil {
			return nil, err
		}
		engine.Local = &EngineLocal{Via: "core", Model: str(l.Model)}
	}

	actions := strs(m.Actions)
	for _, a := range actions {
		au := strings.SplitN(a, ".", 2)[0]
		if err := req(au == unit || au == "group", `%s: action "%s" belongs to unit "%s", not "%s"`, where, a, au, unit); err != nil {
			return nil, err
		}
	}
	secrets := strs(m.Secrets)
	for _, s := range secrets {
		if err := req(envNameRe.MatchString(s), `%s: secret "%s" must be an env var NAME`, where, s); err != nil {
			return nil, err
		}
	}
	optional := []OptionalSecret{}
	for _, o := range m.OptionalSecrets {
		name := str(o["name"])
		if err := req(envNameRe.MatchString(name), "%s: optional_secrets entries need an env var NAME (name: ...)", where); err != nil {
			return nil, err
		}
		optional = append(optional, OptionalSecret{Name: name, Skills: orEmpty(anyStrs(o["skills"])), Actions: orEmpty(anyStrs(o["actions"]))})
	}
	for _, o := range optional {
		if err := req(!slices.Contains(secrets, o.Name), "%s: %s is under both secrets: and optional_secrets:", where, o.Name); err != nil {
			return nil, err
		}
		n := 0
		for _, x := range optional {
			if x.Name == o.Name {
				n++
			}
		}
		if err := req(n == 1, "%s: optional secret %s is listed twice", where, o.Name); err != nil {
			return nil, err
		}
		for _, sk := range o.Skills {
			if err := req(exists(filepath.Join(AgentDir(root, id), "skills", sk, "SKILL.md")), `%s: optional secret %s gates skill "%s", which is not in agents/%s/skills/`, where, o.Name, sk, id); err != nil {
				return nil, err
			}
		}
		for _, a := range o.Actions {
			if err := req(slices.Contains(actions, a), `%s: optional secret %s gates action "%s", which is not under actions:`, where, o.Name, a); err != nil {
				return nil, err
			}
		}
	}
	declared := func(n string) bool {
		return slices.Contains(secrets, n) || slices.ContainsFunc(optional, func(o OptionalSecret) bool { return o.Name == n })
	}
	for _, s := range LocalOnlySecrets(engine) {
		if err := req(!declared(s), "%s: %s is local-only (engine.local); do not declare it under secrets:", where, s); err != nil {
			return nil, err
		}
	}

	envs := env.New()
	isMap := m.Env.Kind == yaml.MappingNode
	if err := req(m.Env.Kind == 0 || isMap || m.Env.Tag == "!!null", "%s: env must be a map of NAME: value", where); err != nil {
		return nil, err
	}
	for i := 0; isMap && i+1 < len(m.Env.Content); i += 2 {
		k, vn := m.Env.Content[i].Value, m.Env.Content[i+1]
		if err := req(envNameRe.MatchString(k), `%s: env key "%s" must be an env var NAME`, where, k); err != nil {
			return nil, err
		}
		if err := req(!credRe.MatchString(k), "%s: env %s looks like a credential; put its NAME under secrets: instead", where, k); err != nil {
			return nil, err
		}
		if err := req(!declared(k), "%s: %s is a secret; it cannot also have a value under env:", where, k); err != nil {
			return nil, err
		}
		if err := req(!slices.ContainsFunc(m.State, func(s State) bool { return s.Env == k }), "%s: %s is set by state:; do not repeat it under env:", where, k); err != nil {
			return nil, err
		}
		scalar := vn.Kind == yaml.ScalarNode && (vn.Tag == "!!str" || vn.Tag == "!!int" || vn.Tag == "!!float" || vn.Tag == "!!bool")
		if err := req(scalar, "%s: env %s must be a string, number or boolean", where, k); err != nil {
			return nil, err
		}
		envs.Set(k, vn.Value)
	}

	channels := []Channel{}
	for _, c := range m.Channels {
		kind := str(c["kind"])
		needs, known := ChannelSecrets[kind]
		if err := req(known, `%s: unknown channel kind "%s" (known: slack, telegram)`, where, kind); err != nil {
			return nil, err
		}
		for _, n := range needs {
			if err := req(declared(n), "%s: channel %s needs secret %s under secrets: or optional_secrets:", where, kind, n); err != nil {
				return nil, err
			}
		}
		okBools := true
		ch := Channel{Kind: kind, SlackSettings: settings(c, &okBools)}
		if l, ok := c["local"].(map[string]any); ok {
			s := settings(l, &okBools)
			ch.Local = &s
		}
		users := append([]string{}, ch.AllowedUsers...)
		if ch.Local != nil {
			users = append(users, ch.Local.AllowedUsers...)
		}
		for _, u := range users {
			if kind == "slack" {
				if err := req(memberRe.MatchString(u), `%s: slack allowed_users entry "%s" is not a Member ID (U…)`, where, u); err != nil {
					return nil, err
				}
			} else if err := req(numericRe.MatchString(u), `%s: telegram allowed_users entry "%s" is not a numeric user id`, where, u); err != nil {
				return nil, err
			}
		}
		if ch.AllowBots != "" {
			if err := req(kind == "slack" && slices.Contains([]string{"none", "mentions", "all"}, ch.AllowBots), "%s: allow_bots is slack-only and must be none|mentions|all", where); err != nil {
				return nil, err
			}
		}
		if err := req(okBools, "%s: allow_all_users must be true or false", where); err != nil {
			return nil, err
		}
		if v, set := c["slash_commands"]; set && v != nil {
			b, isBool := v.(bool)
			if err := req(kind == "slack" && isBool, "%s: slash_commands is slack-only and must be true or false", where); err != nil {
				return nil, err
			}
			ch.SlashCommands = &b
		}
		channels = append(channels, ch)
	}

	learning := Learning{MemoryCharLimit: 2200, UserCharLimit: 1375, SeedFill: 0.6, NapIntervalSeconds: 900}
	if v, ok := num(m.Learning["memory_char_limit"]); ok {
		learning.MemoryCharLimit = int(v)
	}
	if v, ok := num(m.Learning["user_char_limit"]); ok {
		learning.UserCharLimit = int(v)
	}
	if v, ok := num(m.Learning["seed_fill"]); ok {
		learning.SeedFill = v
	}
	if v, ok := num(m.Learning["nap_interval_seconds"]); ok {
		learning.NapIntervalSeconds = int(v)
	}
	deploy := Deploy{Launch: "fargate", Registry: "ghcr", CPU: 1024, Memory: 2048, EphemeralStorageGiB: 30, SecretID: secretPrefix + "/" + id}
	if v := str(m.Deploy["launch"]); v != "" {
		deploy.Launch = v
	}
	if v := str(m.Deploy["registry"]); v != "" {
		deploy.Registry = v
	}
	if v, ok := num(m.Deploy["cpu"]); ok {
		deploy.CPU = int(v)
	}
	if v, ok := num(m.Deploy["memory"]); ok {
		deploy.Memory = int(v)
	}
	if v, ok := num(m.Deploy["ephemeral_storage_gib"]); ok {
		deploy.EphemeralStorageGiB = int(v)
	}
	if v := str(m.Deploy["secret_id"]); v != "" {
		deploy.SecretID = v
	}
	var persona *Persona
	if m.Persona != nil {
		persona = &Persona{Repo: m.Persona.Repo, Path: m.Persona.Path}
	}
	state := m.State
	if state == nil {
		state = []State{}
	}
	return &Agent{
		ID: mid, Name: str(m.Name), Unit: unit, Role: str(m.Role), Persona: persona, Engine: engine,
		Channels: channels, Secrets: secrets, OptionalSecrets: optional, Actions: actions, Env: envs,
		State: state, Learning: learning, Deploy: deploy,
	}, nil
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func num(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// PersonaInInstance reports whether the persona lives in the instance itself (`repo` omitted, or
// naming the instance's resource name).
func PersonaInInstance(p Persona, resource string) bool { return p.Repo == "" || p.Repo == resource }

// Off is an optional secret without a value, with what it switched off (for status output).
type Off struct {
	Name string   `json:"name"`
	What []string `json:"what"`
}

// Effective is the manifest as it runs: `Secrets` holds the required names plus the optional ones
// that are present (OptionalSecrets is then empty); actions and channels needing an absent one are gone.
type Effective struct {
	Agent *Agent `json:"agent"`
	// Skill paths (under agents/<id>/skills/) to leave out of the build.
	SkipSkills []string `json:"skipSkills"`
	Off        []Off    `json:"off"`
}

// ApplyOptional applies optional_secrets for the set of secret names that have values.
func ApplyOptional(a *Agent, present []string) Effective {
	have := map[string]bool{}
	for _, p := range present {
		have[p] = true
	}
	absentNames := map[string]bool{}
	dropActions := map[string]bool{}
	skip := []string{}
	off := []Off{}
	cp := *a
	cp.Secrets = append([]string{}, a.Secrets...)
	for _, o := range a.OptionalSecrets {
		if have[o.Name] {
			cp.Secrets = append(cp.Secrets, o.Name)
			continue
		}
		absentNames[o.Name] = true
		what := []string{}
		for _, c := range a.Channels {
			if slices.Contains(ChannelSecrets[c.Kind], o.Name) {
				what = append(what, c.Kind+" channel")
			}
		}
		for _, sk := range o.Skills {
			what = append(what, "skill "+sk)
			if !slices.Contains(skip, sk) {
				skip = append(skip, sk)
			}
		}
		for _, ac := range o.Actions {
			what = append(what, "action "+ac)
			dropActions[ac] = true
		}
		off = append(off, Off{Name: o.Name, What: what})
	}
	cp.OptionalSecrets = []OptionalSecret{}
	cp.Actions = []string{}
	for _, ac := range a.Actions {
		if !dropActions[ac] {
			cp.Actions = append(cp.Actions, ac)
		}
	}
	cp.Channels = []Channel{}
	for _, c := range a.Channels {
		if !slices.ContainsFunc(ChannelSecrets[c.Kind], func(n string) bool { return absentNames[n] }) {
			cp.Channels = append(cp.Channels, c)
		}
	}
	return Effective{Agent: &cp, SkipSkills: skip, Off: off}
}
