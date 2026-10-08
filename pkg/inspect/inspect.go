// Package inspect is `stormo inspect`: everything the engine computes for one agent, as one JSON
// document, without writing anything. Instance tests and people read it to check what an agent
// would run with (pinned names, env, files, task definition) in any language.
package inspect

import (
	"sort"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/deploy"
	"github.com/camfinc/stormo/pkg/env"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"go.yaml.in/yaml/v3"
)

// Report is the inspect document.
type Report struct {
	Instance *instance.Instance `json:"instance"`
	Target   manifest.Target    `json:"target"`
	// The manifest as it runs (optional secrets applied).
	Agent      *manifest.Agent `json:"agent"`
	SkipSkills []string        `json:"skipSkills"`
	Off        []manifest.Off  `json:"off"`
	Image      string          `json:"image"`
	Env        *env.Env        `json:"env"`
	// Compiled baseline: file → sha256, skill dir → hash.
	Files  build.OrderedHashes `json:"files"`
	Skills build.OrderedHashes `json:"skills"`
	// The engine config as compiled (parsed).
	Config map[string]any `json:"config"`
	// AWS only: what `deploy render` would write, and the deploy settings still unset.
	TaskDef    *deploy.TaskDef `json:"taskdef,omitempty"`
	Policy     *deploy.Policy  `json:"policy,omitempty"`
	Unresolved []string        `json:"unresolved,omitempty"`
}

// Agent inspects one agent for a target. present nil reads secrets.local.yaml as a build would.
func Agent(inst *instance.Instance, id string, target manifest.Target, present []string) (*Report, error) {
	r, err := build.Compile(inst, id, build.Options{Target: target, Present: present})
	if err != nil {
		return nil, err
	}
	declared, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	presentNames := present
	if presentNames == nil {
		presentNames = r.Agent.Secrets
	}
	eff := manifest.ApplyOptional(declared, presentNames)
	var cfg map[string]any
	if body, ok := r.Files["config.yaml"]; ok {
		if err := yaml.Unmarshal(body, &cfg); err != nil {
			return nil, err
		}
	}
	rep := &Report{
		Instance: inst, Target: target, Agent: r.Agent, SkipSkills: eff.SkipSkills, Off: r.Off,
		Image: r.Engine.Image(r.Agent), Env: r.Engine.Env(r.Agent, target),
		Files: r.Info.Files, Skills: r.Info.Skills, Config: cfg,
	}
	if rep.SkipSkills == nil {
		rep.SkipSkills = []string{}
	}
	if rep.Off == nil {
		rep.Off = []manifest.Off{}
	}
	sort.Strings(rep.SkipSkills)
	if target == manifest.AWS {
		t := deploy.DefaultTarget(inst)
		td := deploy.RenderTaskDef(r.Agent, r.Engine, t)
		pol := deploy.RenderTaskPolicy(r.Agent, t)
		rep.TaskDef, rep.Policy, rep.Unresolved = &td, &pol, deploy.Unresolved(r.Agent, t)
	}
	return rep, nil
}
