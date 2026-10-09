// Package build compiles an agent's baseline: the engine home a fresh task starts from, plus
// .swarm/baseline.json (file and per-skill hashes rehydrate compares naps against).
package build

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
	"github.com/camfinc/stormo/pkg/shared"
)

// InfoPath is where the baseline records what it is, relative to the baseline dir.
const InfoPath = ".swarm/baseline.json"

// Info is .swarm/baseline.json. Field order is the file format.
type Info struct {
	Agent  string `json:"agent"`
	Unit   string `json:"unit"`
	Engine string `json:"engine"`
	// local baselines carry the manifest's engine.local model and are never shipped.
	Target  manifest.Target `json:"target"`
	BuiltAt string          `json:"builtAt"`
	GitSha  string          `json:"gitSha"`
	// skill dir (relative to skillsDir) → hash of its files; rehydrate compares naps against it.
	Skills OrderedHashes `json:"skills"`
	// Ledger ids a reviewer rejected; rehydrate purges matching entries from live memory.
	Rejected []string `json:"rejected"`
	// Skill dirs left out because an optional secret they need is absent. Rehydrate must not bring
	// them back from an older nap (it would read them as skills the agent wrote itself).
	Withheld []string `json:"withheld,omitempty"`
	// Optional secrets that were absent for this build.
	OptionalOff []string      `json:"optionalOff,omitempty"`
	Files       OrderedHashes `json:"files"`
}

// Sha256 is the hex digest used for every baseline and nap hash.
func Sha256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// SkillDirs groups files under skillsDir/ by the deepest ancestor directory holding a SKILL.md.
func SkillDirs(paths []string, skillsDir string) map[string][]string {
	prefix := skillsDir + "/"
	roots := []string{}
	for _, p := range paths {
		if strings.HasPrefix(p, prefix) && strings.HasSuffix(p, "/SKILL.md") {
			d := strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/SKILL.md")
			hidden := false
			for _, seg := range strings.Split(d, "/") {
				if strings.HasPrefix(seg, ".") {
					hidden = true
				}
			}
			if !hidden {
				roots = append(roots, d)
			}
		}
	}
	sort.SliceStable(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	out := map[string][]string{}
	for _, r := range roots {
		out[r] = []string{}
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rel := strings.TrimPrefix(p, prefix)
		for _, r := range roots {
			if strings.HasPrefix(rel, r+"/") {
				out[r] = append(out[r], p)
				break
			}
		}
	}
	return out
}

// SkillHash hashes a skill dir: sha256 of "<path>\0<file hash>" lines, paths sorted.
func SkillHash(hashes map[string]string, paths []string) string {
	sorted := append([]string{}, paths...)
	sort.Strings(sorted)
	lines := make([]string, len(sorted))
	for i, p := range sorted {
		lines[i] = p + "\x00" + hashes[p]
	}
	return Sha256([]byte(strings.Join(lines, "\n")))
}

func gitSha(root string) string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "uncommitted"
	}
	return strings.TrimSpace(string(out))
}

// Out is where a target's baseline goes by default. The sidecar image copies dist/, so only AWS
// builds go there.
func Out(root, agentID string, target manifest.Target) string {
	if target == manifest.Local {
		return filepath.Join(root, ".swarm", "local", agentID, "baseline")
	}
	return filepath.Join(root, "dist", agentID, "baseline")
}

// Options for Agent.
type Options struct {
	Out    string
	Target manifest.Target
	// Present names the secrets with values (deciding optional secrets); nil reads them from
	// secrets.local.yaml for the target.
	Present []string
}

// Result of a build.
type Result struct {
	Out    string
	Info   *Info
	Files  map[string][]byte
	Agent  *manifest.Agent
	Engine engine.Engine
	Off    []manifest.Off
}

// Compile produces the baseline in memory without writing it.
func Compile(inst *instance.Instance, agentID string, o Options) (*Result, error) {
	if o.Target == "" {
		o.Target = manifest.AWS
	}
	declared, err := manifest.Load(inst.Root, agentID, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	present := o.Present
	if present == nil {
		if present, err = secrets.Present(inst.Root, declared, o.Target); err != nil {
			return nil, err
		}
	}
	eff := manifest.ApplyOptional(declared, present)
	eng, err := engines.Get(eff.Agent.Engine.Kind, inst)
	if err != nil {
		return nil, err
	}
	if err := manifest.CheckConnections(declared, inst); err != nil {
		return nil, err
	}
	ctx, err := learning.CompileLearning(inst, agentID, eng.Memory())
	if err != nil {
		return nil, err
	}
	ctx.Target = o.Target
	ctx.SkipSkills = eff.SkipSkills
	if o.Target == manifest.Local && eff.Agent.Engine.Local != nil {
		// On the swarm core the shared space is coordinated by it: locks, attribution, workdir.py.
		for k, v := range shared.LocalSkill(inst, eff.Agent) {
			ctx.Knowledge[k] = v
		}
	}
	files, err := eng.Compile(eff.Agent, ctx)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	hashes := map[string]string{}
	fileHashes := OrderedHashes{}
	for _, p := range paths {
		hashes[p] = Sha256(files[p])
		fileHashes = append(fileHashes, KV{p, hashes[p]})
	}
	dirs := SkillDirs(paths, eng.Layout().SkillsDir)
	dirNames := make([]string, 0, len(dirs))
	for d := range dirs {
		dirNames = append(dirNames, d)
	}
	sort.Strings(dirNames)
	skills := OrderedHashes{}
	for _, d := range dirNames {
		skills = append(skills, KV{d, SkillHash(hashes, dirs[d])})
	}
	ledger, err := learning.LoadLedger(inst.Root, agentID)
	if err != nil {
		return nil, err
	}
	rejected := []string{}
	for _, e := range ledger {
		if e.Status == learning.Rejected {
			rejected = append(rejected, e.ID)
		}
	}
	info := &Info{
		Agent: eff.Agent.ID, Unit: eff.Agent.Unit, Engine: eng.Kind(), Target: o.Target,
		BuiltAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), GitSha: gitSha(inst.Root),
		Skills: skills, Rejected: rejected, Files: fileHashes,
	}
	if len(eff.SkipSkills) > 0 {
		info.Withheld = eff.SkipSkills
	}
	for _, off := range eff.Off {
		info.OptionalOff = append(info.OptionalOff, off.Name)
	}
	out := o.Out
	if out == "" {
		out = Out(inst.Root, agentID, o.Target)
	}
	return &Result{Out: out, Info: info, Files: files, Agent: eff.Agent, Engine: eng, Off: eff.Off}, nil
}

// Agent compiles an agent's baseline and writes it (replacing the directory).
func Agent(inst *instance.Instance, agentID string, o Options) (*Result, error) {
	r, err := Compile(inst, agentID, o)
	if err != nil {
		return nil, err
	}
	if err := os.RemoveAll(r.Out); err != nil {
		return nil, err
	}
	for p, body := range r.Files {
		dst := filepath.Join(r.Out, p)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			return nil, err
		}
	}
	info, err := learning.MarshalIndent(r.Info)
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(r.Out, InfoPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	return r, os.WriteFile(dst, info, 0o644)
}
