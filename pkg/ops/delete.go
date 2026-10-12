package ops

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
)

// DeleteOptions are `stormo delete agent`'s choices.
type DeleteOptions struct {
	// Persona also removes the agent's persona folder when no other agent uses it and it lives in
	// this instance (not a sibling repo).
	Persona bool
}

// DeleteResult is what DeleteAgent removed and what it left.
type DeleteResult struct {
	Agent   string   `json:"agent"`
	Removed []string `json:"removed"`
	// Notes are what stays and may need a person: a persona kept, an ECS service, an empty unit.
	Notes []string `json:"notes"`
}

// DeleteAgent removes agent id from the instance: its folder (definition, skills, learnings, data:
// naps and secret values), its own secret layers in secrets.local.yaml, its generated build and
// compose files and its port, and optionally its persona. A local container is stopped first. It
// refuses while the agent runs on ECS and never deletes AWS resources. The core's history about
// the agent (activity, messages) stays.
func DeleteAgent(d *Deps, id string, o DeleteOptions) (*DeleteResult, error) {
	root := d.Inst.Root
	if !slices.Contains(manifest.AgentIDs(root), id) {
		return nil, fmt.Errorf("no agent %q in this instance", id)
	}
	a, err := manifest.Load(root, id, d.Inst.Names.Secret)
	if err != nil {
		return nil, fmt.Errorf("%s: %w (fix agent.yaml, or remove agents/%s by hand)", id, err, id)
	}
	r := &DeleteResult{Agent: id, Removed: []string{}, Notes: []string{}}

	if d.Inst.Aws.Region != instance.Unset && d.Inst.Aws.Cluster != instance.Unset {
		switch state, detail := d.remoteState(id); state {
		case "running", "starting":
			return nil, fmt.Errorf("%s runs on ECS (%s): stop it there first (stormo stop %s --remote)", id, detail, id)
		case "stopped":
			r.Notes = append(r.Notes, "its ECS service (stopped) and Secrets Manager entry stay: remove them in AWS if no longer needed")
		case "unknown":
			r.Notes = append(r.Notes, "could not check ECS ("+detail+"): if it runs there, it keeps running from its deployed image")
		}
	}

	if _, err := os.Stat(local.ComposePath(root, a)); err == nil {
		if err := local.Down(d.Inst, id); err != nil {
			return nil, fmt.Errorf("%s: stopping its local container: %w", id, err)
		}
		r.Removed = append(r.Removed, "local container (stopped)")
	}

	// Secrets before the folder: Save must not write the agent's layers back into it.
	f, err := secrets.Load(secrets.Path(root))
	if err != nil {
		return nil, err
	}
	had := f.Agents.Get(id) != nil || (f.Local != nil && f.Local.Agents.Get(id) != nil)
	inFolder := f.InFolder(id)
	if had {
		f.ForgetAgent(id)
		if !inFolder {
			if err := secrets.Save(secrets.Path(root), d.Inst.Names.Resource, f); err != nil {
				return nil, err
			}
			r.Removed = append(r.Removed, "its secret values in secrets.local.yaml")
		}
	}

	persona := ""
	if o.Persona && a.Persona != nil && a.Persona.Repo == "" && a.Persona.Path != "" {
		persona = filepath.Clean(a.Persona.Path)
		for _, other := range manifest.AgentIDs(root) {
			if other == id {
				continue
			}
			if b, err := manifest.Load(root, other, d.Inst.Names.Secret); err == nil && b.Persona != nil && b.Persona.Repo == "" && filepath.Clean(b.Persona.Path) == persona {
				r.Notes = append(r.Notes, persona+" kept: "+other+" uses it too")
				persona = ""
				break
			}
		}
		if persona != "" && (!strings.HasPrefix(persona, "personas/") || strings.Contains(persona, "..")) {
			r.Notes = append(r.Notes, persona+" kept: not a folder under personas/")
			persona = ""
		}
	} else if a.Persona != nil && a.Persona.Path != "" {
		r.Notes = append(r.Notes, "persona "+a.Persona.Path+" kept")
	}

	gone := []string{
		"agents/" + id,
		".swarm/compose/" + id + ".yaml",
		".swarm/local/" + id,
		".swarm/check/" + id,
		".swarm/store/" + id,
		".swarm/slack/" + id,
		".swarm/env/" + id + ".env",
		"agents/.import-" + id,
		"agents/.replaced-" + id,
		"dist/" + id,
	}
	if persona != "" {
		gone = append(gone, persona)
	}
	for _, rel := range gone {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return r, fmt.Errorf("removing %s: %w", rel, err)
		}
		r.Removed = append(r.Removed, rel)
	}
	if err := local.ReleasePort(root, id); err != nil {
		return r, err
	}

	if !slices.ContainsFunc(manifest.AgentIDs(root), func(other string) bool {
		b, err := manifest.Load(root, other, d.Inst.Names.Secret)
		return err == nil && b.Unit == a.Unit
	}) {
		r.Notes = append(r.Notes, "unit "+a.Unit+" has no agents left (units/"+a.Unit+" stays)")
	}
	return r, nil
}
