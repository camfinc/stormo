// Package shared is the shared document space. Every agent task mounts two layers of one EFS file
// system: /shared/group (read-write for every agent) and /shared/<unit> (that unit's agents only).
// Other units' directories are never mounted, so isolation is structural, not a convention. The
// space lives outside the engine home: naps neither snapshot nor restore it, and the dream never
// harvests it into git.
package shared

import (
	_ "embed"
	"strings"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// Root is where the layers are mounted inside the agent container.
const Root = "/shared"

// PosixUID: EFS access points run as the Hermes image's runtime user so files stay writable by every agent.
const PosixUID = 10000

// Layer is one mounted directory: "group" or a unit id.
type Layer struct {
	Layer string `json:"layer"`
	Path  string `json:"path"`
}

// Layers are the two directories an agent mounts.
func Layers(a *manifest.Agent) []Layer {
	return []Layer{{"group", Root + "/group"}, {a.Unit, Root + "/" + a.Unit}}
}

//go:embed templates/SKILL.md
var skillTemplate string

//go:embed templates/shared_docs.py
var scriptTemplate string

// Skill is the generated skill (path relative to the skills dir → content) teaching the agent the
// shared space, from templates whose zz…zz markers are the instance's and agent's names.
func Skill(inst *instance.Instance, a *manifest.Agent) map[string]string {
	r := strings.NewReplacer(
		"zzsharedzz", inst.Names.SharedSkill,
		"zzknowledgezz", inst.Names.KnowledgeSkill,
		"zzresourcezz", inst.Names.Resource,
		"zzslugzz", inst.Slug,
		"ZZORGZZ", inst.Org,
		"zzunitzz", a.Unit,
		"zzagentzz", a.ID,
	)
	return map[string]string{
		inst.Names.SharedSkill + "/SKILL.md":               r.Replace(skillTemplate),
		inst.Names.SharedSkill + "/scripts/shared_docs.py": r.Replace(scriptTemplate),
	}
}
