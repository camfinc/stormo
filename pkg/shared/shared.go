// Package shared is the shared document space. Every agent task mounts two layers of one EFS file
// system: /shared/group (read-write for every agent) and /shared/<unit> (that unit's agents only).
// Other units' directories are never mounted, so isolation is structural, not a convention. The
// space lives outside the engine home: naps neither snapshot nor restore it, and the dream never
// harvests it into git.
package shared

import (
	_ "embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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

// LocalDir is the shared space of local runs: workdir/ at the instance root, one directory per
// layer (workdir/group, workdir/<unit>), each mounted only into the agents that may see it, at the
// same /shared/<layer> path as on AWS. The core tracks it (docs/core.md §4). Gitignored.
func LocalDir(root string) string { return filepath.Join(root, "workdir") }

// legacyDir is where the local shared space lived before workdir/.
func legacyDir(root string) string { return filepath.Join(root, ".swarm", "shared") }

// MigrateLocal moves each layer of the old local shared space (.swarm/shared/<layer>) to
// workdir/<layer> when workdir/ has none yet, and leaves a symlink at the old path, so an agent
// still running with the old mount keeps writing into the same directory until it restarts.
// Layers present in both places are left alone (reported, never merged).
func MigrateLocal(root string) (moved, conflicts []string, err error) {
	des, err := os.ReadDir(legacyDir(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, d := range des {
		if !d.IsDir() {
			continue // already a symlink to workdir/, or a stray file
		}
		from := filepath.Join(legacyDir(root), d.Name())
		to := filepath.Join(LocalDir(root), d.Name())
		if _, err := os.Lstat(to); err == nil {
			conflicts = append(conflicts, d.Name())
			continue
		}
		if err := os.MkdirAll(LocalDir(root), 0o755); err != nil {
			return moved, conflicts, err
		}
		if err := os.Rename(from, to); err != nil {
			return moved, conflicts, err
		}
		if err := os.Symlink(filepath.Join("..", "..", "workdir", d.Name()), from); err != nil {
			return moved, conflicts, err
		}
		moved = append(moved, d.Name())
	}
	return moved, conflicts, nil
}

// Layers are the two directories an agent mounts.
func Layers(a *manifest.Agent) []Layer {
	return []Layer{{"group", Root + "/group"}, {a.Unit, Root + "/" + a.Unit}}
}

//go:embed templates/SKILL.md
var skillTemplate string

//go:embed templates/shared_docs.py
var scriptTemplate string

//go:embed templates/WORKDIR.md
var workdirSection string

//go:embed templates/workdir.py
var workdirScript string

// Skill is the generated skill (path relative to the skills dir → content) teaching the agent the
// shared space, from templates whose zz…zz markers are the instance's and agent's names.
func Skill(inst *instance.Instance, a *manifest.Agent) map[string]string {
	r := replacer(inst, a)
	return map[string]string{
		inst.Names.SharedSkill + "/SKILL.md":               r.Replace(skillTemplate),
		inst.Names.SharedSkill + "/scripts/shared_docs.py": r.Replace(scriptTemplate),
	}
}

// LocalSkill is Skill for a local run on the swarm core: the same skill, plus how to coordinate
// through the core (locks, attribution) and its workdir.py helper.
func LocalSkill(inst *instance.Instance, a *manifest.Agent) map[string]string {
	r := replacer(inst, a)
	files := Skill(inst, a)
	files[inst.Names.SharedSkill+"/SKILL.md"] += r.Replace(workdirSection)
	files[inst.Names.SharedSkill+"/scripts/workdir.py"] = r.Replace(workdirScript)
	return files
}

func replacer(inst *instance.Instance, a *manifest.Agent) *strings.Replacer {
	return strings.NewReplacer(
		"zzsharedzz", inst.Names.SharedSkill,
		"zzknowledgezz", inst.Names.KnowledgeSkill,
		"zzresourcezz", inst.Names.Resource,
		"zzslugzz", inst.Slug,
		"ZZORGZZ", inst.Org,
		"zzunitzz", a.Unit,
		"zzagentzz", a.ID,
	)
}
