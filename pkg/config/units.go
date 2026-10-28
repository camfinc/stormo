package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// UnitInfo is one unit as an editor lists it (`config units`).
type UnitInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"` // its unit.yaml, relative to the instance
	// Problem is why its unit.yaml does not load; the unit is still listed, to be fixed as text.
	Problem string `json:"problem,omitempty"`
	// Agents are the agents its knowledge reaches: those in the unit, every agent for group.
	Agents    []string        `json:"agents"`
	Knowledge []KnowledgeFile `json:"knowledge"`
}

// KnowledgeFile is one Markdown file in units/<u>/knowledge/.
type KnowledgeFile struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	// Compiled is false for README.md: it is for people and never reaches an agent.
	Compiled bool `json:"compiled"`
}

// Units lists the instance's units, group first.
func Units(inst *instance.Instance) []UnitInfo {
	members := map[string][]string{}
	all := manifest.AgentIDs(inst.Root)
	for _, id := range all {
		var a struct {
			Unit string `yaml:"unit"`
		}
		if b, err := os.ReadFile(filepath.Join(manifest.AgentDir(inst.Root, id), "agent.yaml")); err == nil && yaml.Unmarshal(b, &a) == nil {
			members[a.Unit] = append(members[a.Unit], id)
		}
	}
	ids := manifest.UnitIDs(inst.Root)
	sort.SliceStable(ids, func(i, j int) bool { return ids[i] == "group" && ids[j] != "group" })
	out := []UnitInfo{}
	for _, id := range ids {
		u := UnitInfo{ID: id, Name: id, Path: "units/" + id + "/unit.yaml", Agents: members[id], Knowledge: knowledge(inst.Root, id)}
		if id == "group" {
			u.Agents = all
		}
		if u.Agents == nil {
			u.Agents = []string{}
		}
		if m, err := manifest.LoadUnit(inst.Root, id); err != nil {
			u.Problem = err.Error()
		} else {
			u.Name, u.Description = m.Name, m.Description
			if u.Name == "" {
				u.Name = id
			}
		}
		out = append(out, u)
	}
	return out
}

func knowledge(root, unit string) []KnowledgeFile {
	entries, _ := os.ReadDir(filepath.Join(root, "units", unit, "knowledge"))
	out := []KnowledgeFile{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || !knowledgeName.MatchString(e.Name()) {
			continue
		}
		out = append(out, KnowledgeFile{
			Path:     "units/" + unit + "/knowledge/" + e.Name(),
			Name:     e.Name(),
			Size:     info.Size(),
			Compiled: e.Name() != "README.md",
		})
	}
	return out
}

// Create writes a new unit (units/<id>/unit.yaml, id a slug) or a new knowledge file of an
// existing unit, refused with exists when the file is there. The body is validated as Write
// validates it, and the file appears whole or not at all.
func Create(inst *instance.Instance, rel string, body []byte) (*File, error) {
	t, ok := parse(rel)
	switch {
	case !ok || t.unit == "":
		return nil, refuse("unsupported", "%s: config create makes units/<unit>/unit.yaml or units/<unit>/knowledge/<name>.md", rel)
	case t.kind == "unit" && !instance.ValidSlug(t.unit):
		return nil, refuse("invalid", "%s: a unit id is lowercase letters, digits and hyphens", t.rel)
	case t.kind == "knowledge" && !slices.Contains(manifest.UnitIDs(inst.Root), t.unit):
		return nil, refuse("unsupported", "%s: there is no unit %q", t.rel, t.unit)
	}
	p := t.abs(inst)
	if _, err := os.Lstat(p); err == nil {
		return nil, refuse("exists", "%s already exists", t.rel)
	}
	if err := validate(inst, t, nil, body); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	// A link, not a rename: it fails instead of replacing a file created meanwhile.
	if err := os.Link(tmp.Name(), p); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil, refuse("exists", "%s already exists", t.rel)
		}
		return nil, err
	}
	return file(inst, t, body), nil
}

// Delete removes a knowledge file if it still hashes to ifHash. A unit itself is not deleted here:
// agents, secrets, the office and the shared space name it.
func Delete(inst *instance.Instance, rel, ifHash string) error {
	t, err := resolve(inst, rel)
	if err != nil {
		return err
	}
	if t.kind != "knowledge" {
		return refuse("unsupported", "%s: config delete removes a unit's knowledge file only", t.rel)
	}
	if ifHash == "" {
		return refuse("usage", "%s: --if-hash is required (the hash `config show` gave)", t.rel)
	}
	p := t.abs(inst)
	old, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if Hash(old) != ifHash {
		return refuse("conflict", "%s changed on disk since it was loaded; reload it and edit again", t.rel)
	}
	return os.Remove(p)
}
