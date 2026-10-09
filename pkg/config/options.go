package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/pkg/bridge"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// Options are the choices an agent.yaml form offers, from the instance and the engine, so the
// form never hard-codes what the engine decides.
type Options struct {
	Units     []manifest.Unit `json:"units"`
	Actions   []ActionOption  `json:"actions"`   // bridge actions; an agent may use its unit's and group's
	Skills    []string        `json:"skills"`    // the agent's skills (dirs with a SKILL.md), for optional secrets
	Personas  []string        `json:"personas"`  // personas/<slug> with a persona.md, in this instance
	Engines   []string        `json:"engines"`   // engine.kind
	Channels  []ChannelOption `json:"channels"`  // channels[].kind and the secrets each needs declared
	AllowBots []string        `json:"allowBots"` // channels[].allow_bots (slack)
	Secrets   []string        `json:"secrets"`   // secret names declared anywhere in the instance (names only)
	Scripts   []string        `json:"scripts"`   // the agent's scripts/, for schedules' script and monitor
	Reasoning []string        `json:"reasoning"` // limits.reasoning levels the agent's engine accepts
	// Connections the agent may use: API ones for model.provider, ChatGPT ones for model.local.
	Connections []ConnectionOption `json:"connections"`
}

type ConnectionOption struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	API   bool   `json:"api"`
	// Key is the secret an agent using it declares.
	Key string `json:"key,omitempty"`
}

type ActionOption struct {
	Name        string `json:"name"`
	Unit        string `json:"unit"`
	Description string `json:"description"`
	Mutates     bool   `json:"mutates"`
}

type ChannelOption struct {
	Kind    string   `json:"kind"`
	Secrets []string `json:"secrets"`
}

func options(inst *instance.Instance, agent string) *Options {
	o := &Options{Units: []manifest.Unit{}, Actions: []ActionOption{}, Skills: []string{}, Personas: []string{},
		Engines: engines.Kinds(), Channels: []ChannelOption{}, AllowBots: manifest.AllowBots, Secrets: []string{},
		Scripts: []string{}, Reasoning: []string{}, Connections: []ConnectionOption{}}
	for _, c := range inst.Connections {
		k, _ := instance.KindOf(c.Kind)
		o.Connections = append(o.Connections, ConnectionOption{c.Name, c.Kind, k.Label, k.API, c.Key})
	}
	if a, err := manifest.Load(inst.Root, agent, inst.Names.Secret); err == nil {
		if eng, err := engines.Get(a.Engine.Kind, inst); err == nil {
			o.Reasoning = eng.Reasoning()
		}
	}
	scripts := filepath.Join(manifest.AgentDir(inst.Root, agent), "scripts")
	_ = filepath.WalkDir(scripts, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			rel, _ := filepath.Rel(scripts, p)
			o.Scripts = append(o.Scripts, filepath.ToSlash(rel))
		}
		return nil
	})
	for _, id := range manifest.UnitIDs(inst.Root) {
		if u, err := manifest.LoadUnit(inst.Root, id); err == nil {
			o.Units = append(o.Units, *u)
		} else {
			o.Units = append(o.Units, manifest.Unit{ID: id, Name: id})
		}
	}
	if registry, err := bridge.Load(inst); err == nil {
		for _, a := range registry {
			o.Actions = append(o.Actions, ActionOption{a.Name, strings.SplitN(a.Name, ".", 2)[0], a.Description, a.Mutates})
		}
	}
	skills := filepath.Join(manifest.AgentDir(inst.Root, agent), "skills")
	_ = filepath.WalkDir(skills, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "SKILL.md" {
			rel, _ := filepath.Rel(skills, filepath.Dir(p))
			o.Skills = append(o.Skills, filepath.ToSlash(rel))
		}
		return nil
	})
	entries, _ := os.ReadDir(filepath.Join(inst.Root, "personas"))
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(inst.Root, "personas", e.Name(), "persona.md")); err == nil {
			o.Personas = append(o.Personas, "personas/"+e.Name())
		}
	}
	names := map[string]bool{}
	for kind, needs := range manifest.ChannelSecrets {
		o.Channels = append(o.Channels, ChannelOption{kind, needs})
		for _, n := range needs {
			names[n] = true
		}
	}
	sort.Slice(o.Channels, func(i, j int) bool { return o.Channels[i].Kind < o.Channels[j].Kind })
	for _, id := range manifest.AgentIDs(inst.Root) {
		var m struct {
			Secrets         []string         `yaml:"secrets"`
			OptionalSecrets []map[string]any `yaml:"optional_secrets"`
		}
		b, _ := os.ReadFile(filepath.Join(manifest.AgentDir(inst.Root, id), "agent.yaml"))
		if yaml.Unmarshal(b, &m) != nil {
			continue
		}
		for _, n := range m.Secrets {
			names[n] = true
		}
		for _, s := range m.OptionalSecrets {
			if n, ok := s["name"].(string); ok {
				names[n] = true
			}
		}
	}
	for n := range names {
		o.Secrets = append(o.Secrets, n)
	}
	slices.Sort(o.Secrets)
	return o
}
