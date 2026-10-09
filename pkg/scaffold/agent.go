package scaffold

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/pkg/config"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
)

// AgentSpec is a new agent as `stormo new agent` takes it (JSON on stdin; docs/api.md). Only Name
// is required: the rest defaults from AgentDefaults. Secret values never come here, only names.
type AgentSpec struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	Role string `json:"role,omitempty"`
	Unit string `json:"unit,omitempty"`
	// NewUnit creates units/<id>/unit.yaml first, for an instance with no unit to put the agent in.
	NewUnit  *manifest.Unit `json:"newUnit,omitempty"`
	Persona  string         `json:"persona,omitempty"`
	Engine   EngineChoice   `json:"engine"`
	Model    ModelChoice    `json:"model"`
	Channels []ChannelSpec  `json:"channels,omitempty"`
	// Secrets are extra names to declare (channel, connection and engine keys are added anyway).
	Secrets []string `json:"secrets,omitempty"`
	// Soul is SOUL.md; empty writes a starter from the name and role.
	Soul string `json:"soul,omitempty"`
}

type EngineChoice struct {
	Kind     string `json:"kind,omitempty"`
	Version  string `json:"version,omitempty"`
	ImageTag string `json:"imageTag,omitempty"`
}

type ModelChoice struct {
	Name     string `json:"name,omitempty"`
	Provider string `json:"provider,omitempty"`
	// Local routes the agent through the core's ChatGPT gateway; LocalName empty means no local route.
	LocalName       string `json:"localName,omitempty"`
	LocalConnection string `json:"localConnection,omitempty"`
}

type ChannelSpec struct {
	Kind         string   `json:"kind"`
	AllowedUsers []string `json:"allowedUsers,omitempty"`
	HomeChannel  string   `json:"homeChannel,omitempty"`
	AllowBots    string   `json:"allowBots,omitempty"`
}

// AgentDefaults are what a new agent starts with in this instance: engine and model borrowed from
// the agents already here (the most common choice), the first unit an agent may join.
type AgentDefaults struct {
	Unit   string       `json:"unit"`
	Engine EngineChoice `json:"engine"`
	Model  ModelChoice  `json:"model"`
}

// AgentOptions is `stormo new agent --options`: the choices a new agent's form offers, and the
// defaults it starts from.
type AgentOptions struct {
	Options  *config.Options `json:"options"`
	Defaults AgentDefaults   `json:"defaults"`
	// TakenIDs are the agent ids in use, so a form can refuse one before submitting.
	TakenIDs []string `json:"takenIds"`
}

// NewAgentOptions reads the instance for a new agent's form.
func NewAgentOptions(inst *instance.Instance) AgentOptions {
	return AgentOptions{Options: config.OptionsFor(inst, ""), Defaults: Defaults(inst), TakenIDs: manifest.AgentIDs(inst.Root)}
}

// Defaults picks the instance's usual engine and model; empty fields where there is nothing to borrow.
func Defaults(inst *instance.Instance) AgentDefaults {
	var d AgentDefaults
	for _, u := range manifest.UnitIDs(inst.Root) {
		if u != "group" {
			d.Unit = u
			break
		}
	}
	type pick struct {
		e EngineChoice
		m ModelChoice
	}
	counts := map[pick]int{}
	for _, id := range manifest.AgentIDs(inst.Root) {
		a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			continue
		}
		p := pick{EngineChoice{a.Engine.Kind, a.Engine.Version, a.Engine.ImageTag}, ModelChoice{Name: a.Engine.Model, Provider: a.Engine.Provider}}
		if l := a.Engine.Local; l != nil {
			p.m.LocalName, p.m.LocalConnection = l.Model, l.Connection
		}
		counts[p]++
	}
	best, n := pick{}, 0
	for p, c := range counts {
		// Ties go to the alphabetically first model, so the result is stable.
		if c > n || c == n && p.m.Name+p.e.ImageTag < best.m.Name+best.e.ImageTag {
			best, n = p, c
		}
	}
	d.Engine, d.Model = best.e, best.m
	if d.Engine.Kind == "" {
		if kinds := engines.Kinds(); len(kinds) > 0 {
			d.Engine.Kind = kinds[0]
		}
	}
	if d.Model.Provider == "" {
		d.Model.Provider = instance.KindOpenRouter
	}
	return d
}

// AgentResult is what NewAgent made.
type AgentResult struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Unit  string   `json:"unit"`
	Files []string `json:"files"`
	// Secrets are the declared names; values go in with `stormo secrets set <id> NAME`.
	Secrets []string `json:"secrets"`
	// Missing are the declared names with no local value yet (the generated keys are minted).
	Missing []string `json:"missing"`
}

// NewAgent writes agents/<id>/ (agent.yaml at the current format, SOUL.md, data/secrets.yaml with
// its generated keys) and, when asked, the unit it joins. The result must load and pass the connection checks; otherwise nothing is left.
func NewAgent(inst *instance.Instance, s AgentSpec) (AgentResult, error) {
	if s.Name = strings.TrimSpace(s.Name); s.Name == "" {
		return AgentResult{}, errors.New("a name is required")
	}
	if s.ID == "" {
		s.ID = SlugFrom(s.Name)
	}
	if s.ID == "" || !instance.ValidSlug(s.ID) {
		return AgentResult{}, fmt.Errorf("id %q: lowercase letters, digits and hyphens, starting with a letter", s.ID)
	}
	dir := manifest.AgentDir(inst.Root, s.ID)
	if _, err := os.Stat(dir); err == nil {
		return AgentResult{}, fmt.Errorf("agents/%s already exists", s.ID)
	}
	d := Defaults(inst)
	if s.NewUnit != nil {
		s.Unit = s.NewUnit.ID
	}
	s.Unit = or(s.Unit, d.Unit)
	if s.Engine.Kind == "" {
		s.Engine = d.Engine
	} else if s.Engine.Kind == d.Engine.Kind {
		s.Engine.Version, s.Engine.ImageTag = or(s.Engine.Version, d.Engine.Version), or(s.Engine.ImageTag, d.Engine.ImageTag)
	}
	if s.Model.Name == "" {
		s.Model.Name = d.Model.Name
		if s.Model.LocalName == "" && s.Model.LocalConnection == "" {
			s.Model.LocalName, s.Model.LocalConnection = d.Model.LocalName, d.Model.LocalConnection
		}
	}
	s.Model.Provider = or(s.Model.Provider, d.Model.Provider)
	switch {
	case s.Unit == "":
		return AgentResult{}, errors.New(`no unit to join: agents cannot be in "group"; create one (newUnit)`)
	case s.Engine.Version == "":
		return AgentResult{}, fmt.Errorf("engine %s: no version to borrow from another agent; give one", s.Engine.Kind)
	case s.Model.Name == "":
		return AgentResult{}, errors.New("no model to borrow from another agent; give one")
	}
	eng, err := engines.Get(s.Engine.Kind, inst)
	if err != nil {
		return AgentResult{}, err
	}

	declared := append([]string{}, s.Secrets...)
	if c, ok := inst.Connection(s.Model.Provider); ok && c.Key != "" {
		declared = append(declared, c.Key)
	}
	for _, c := range s.Channels {
		declared = append(declared, manifest.ChannelSecrets[c.Kind]...)
	}
	declared = append(declared, eng.Runtime().APIKeyName())
	sort.Strings(declared)
	declared = slices.Compact(declared)

	var unitFile string
	if u := s.NewUnit; u != nil {
		if !instance.ValidSlug(u.ID) || u.ID == "group" {
			return AgentResult{}, fmt.Errorf("unit id %q: lowercase letters, digits and hyphens, and not group", u.ID)
		}
		if slices.Contains(manifest.UnitIDs(inst.Root), u.ID) {
			return AgentResult{}, fmt.Errorf("units/%s already exists", u.ID)
		}
		unitFile = filepath.Join(inst.Root, "units", u.ID, "unit.yaml")
	}

	body, err := agentYAML(s, declared)
	if err != nil {
		return AgentResult{}, err
	}
	files := map[string]string{"agent.yaml": body, "SOUL.md": soul(s)}
	fail := func(err error) (AgentResult, error) {
		_ = os.RemoveAll(dir)
		if unitFile != "" {
			_ = os.RemoveAll(filepath.Dir(unitFile))
		}
		return AgentResult{}, err
	}
	if unitFile != "" {
		u := s.NewUnit
		text := fmt.Sprintf("id: %s\nname: %s\ndescription: %s\n", u.ID, q(or(u.Name, u.ID)), q(u.Description))
		if err := writeFile(unitFile, text); err != nil {
			return fail(err)
		}
		if err := writeFile(filepath.Join(filepath.Dir(unitFile), "knowledge", "README.md"),
			"# "+or(u.Name, u.ID)+" knowledge\n\nPolicy this unit's agents follow, as Markdown files in this folder.\n"); err != nil {
			return fail(err)
		}
	}
	for rel, text := range files {
		if err := writeFile(filepath.Join(dir, rel), text); err != nil {
			return fail(err)
		}
	}
	a, err := manifest.Load(inst.Root, s.ID, inst.Names.Secret)
	if err != nil {
		return fail(err)
	}
	if err := manifest.CheckConnections(a, inst); err != nil {
		return fail(err)
	}
	r := AgentResult{ID: s.ID, Name: s.Name, Unit: s.Unit, Secrets: declared,
		Files: []string{"agents/" + s.ID + "/agent.yaml", "agents/" + s.ID + "/SOUL.md"}}
	if unitFile != "" {
		r.Files = append(r.Files, "units/"+s.Unit+"/unit.yaml")
	}
	// Its generated keys go in its own data folder; secrets.local.yaml is not rewritten.
	if _, err := secrets.SeedAgent(inst.Root, a); err != nil {
		return fail(err)
	}
	r.Missing = []string{}
	file, err := secrets.Load(secrets.Path(inst.Root))
	if err != nil {
		return fail(err)
	}
	values := secrets.Resolve(file, a, manifest.Local).Values
	for _, n := range declared {
		if v, _ := values.Get(n); v == "" {
			r.Missing = append(r.Missing, n)
		}
	}
	return r, nil
}

func writeFile(p, text string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(text), 0o644)
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// agentYAML renders agent.yaml in the order people read it, with a comment on each part.
func agentYAML(s AgentSpec, secrets []string) (string, error) {
	type local struct {
		Via        string `yaml:"via"`
		Name       string `yaml:"name"`
		Connection string `yaml:"connection,omitempty"`
	}
	type model struct {
		Name     string `yaml:"name"`
		Provider string `yaml:"provider,omitempty"`
		Local    *local `yaml:"local,omitempty"`
	}
	type channel struct {
		Kind         string   `yaml:"kind"`
		AllowBots    string   `yaml:"allow_bots,omitempty"`
		AllowedUsers []string `yaml:"allowed_users,omitempty,flow"`
		HomeChannel  string   `yaml:"home_channel,omitempty"`
	}
	part := func(v any) (string, error) {
		var b strings.Builder
		enc := yaml.NewEncoder(&b)
		enc.SetIndent(2)
		err := enc.Encode(v)
		return b.String(), err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s", s.Name)
	if s.Role != "" {
		fmt.Fprintf(&b, ": %s", strings.Join(strings.Fields(s.Role), " "))
	}
	b.WriteString("\n# Made with `stormo new agent`. Every field: docs/instances.md in the engine.\n")
	head, err := part(struct {
		Format int    `yaml:"format"`
		ID     string `yaml:"id"`
		Name   string `yaml:"name"`
		Unit   string `yaml:"unit"`
		Role   string `yaml:"role,omitempty"`
	}{manifest.Format, s.ID, s.Name, s.Unit, strings.TrimSpace(s.Role)})
	if err != nil {
		return "", err
	}
	b.WriteString(head)
	if s.Persona != "" {
		b.WriteString("\npersona:\n  path: " + q(s.Persona) + "\n")
	}
	m := model{Name: s.Model.Name, Provider: s.Model.Provider}
	if s.Model.LocalName != "" {
		m.Local = &local{"core", s.Model.LocalName, s.Model.LocalConnection}
	}
	rest := []struct {
		comment string
		v       any
	}{
		{"# The engine that runs the agent, pinned (never latest).", map[string]any{"engine": struct {
			Kind     string `yaml:"kind"`
			Version  string `yaml:"version"`
			ImageTag string `yaml:"image_tag,omitempty"`
		}{s.Engine.Kind, s.Engine.Version, s.Engine.ImageTag}}},
		{"# The cloud model, and (local) the core's ChatGPT route to it.", map[string]any{"model": m}},
	}
	if len(s.Channels) > 0 {
		chans := []channel{}
		for _, c := range s.Channels {
			chans = append(chans, channel{c.Kind, c.AllowBots, c.AllowedUsers, c.HomeChannel})
		}
		rest = append(rest, struct {
			comment string
			v       any
		}{"# Where people reach the agent.", map[string]any{"channels": chans}})
	}
	rest = append(rest, struct {
		comment string
		v       any
	}{"# Names only: values go in with `stormo secrets set " + s.ID + " NAME`.", map[string]any{"secrets": secrets}})
	for _, r := range rest {
		text, err := part(r.v)
		if err != nil {
			return "", err
		}
		b.WriteString("\n" + r.comment + "\n" + text)
	}
	return b.String(), nil
}

func soul(s AgentSpec) string {
	if t := strings.TrimSpace(s.Soul); t != "" {
		return t + "\n"
	}
	return StarterSoul(s.Name, s.Role)
}

// StarterSoul is a first SOUL.md for an agent, from its name and role.
func StarterSoul(name, role string) string {
	role = strings.TrimSpace(role)
	if role == "" {
		role = "Describe what " + name + " does here."
	}
	return fmt.Sprintf(`# %s

%s

## How you work

- Be clear and brief. Ask when a request is ambiguous instead of guessing.
- Say what you did and what you could not do.
- Never share credentials, and never act outside what you were asked to do.
`, name, role)
}
