package persona

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/camfinc/stormo/pkg/config"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// AgentLook is an agent's look as `stormo look <agent>` shows it (docs/api.md).
type AgentLook struct {
	Agent string `json:"agent"`
	// Path is the persona folder (instance-relative), empty when the agent has none yet.
	Path string `json:"path"`
	// Hash is the persona.md's (config.Hash), empty when there is none: pass it back to SetAgent.
	Hash string `json:"hash"`
	Look Look   `json:"look"`
	// Editable is false for a persona in a sibling repo, which this instance does not write.
	Editable bool   `json:"editable"`
	Reason   string `json:"reason,omitempty"`
	// Shared names the other agents pointing at the same persona: an edit changes their look too.
	Shared []string `json:"shared"`
}

func refuse(code, format string, a ...any) error {
	return &config.Error{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// ShowAgent reads agent id's look.
func ShowAgent(inst *instance.Instance, id string) (*AgentLook, error) {
	a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	r := &AgentLook{Agent: id, Editable: true, Shared: []string{}}
	if a.Persona == nil || a.Persona.Path == "" {
		return r, nil
	}
	r.Path = a.Persona.Path
	if !manifest.PersonaInInstance(*a.Persona, inst.Names.Resource) {
		r.Editable, r.Reason = false, fmt.Sprintf("its persona lives in the %s repo; edit it there", a.Persona.Repo)
	}
	for _, other := range manifest.AgentIDs(inst.Root) {
		if other == id {
			continue
		}
		if o, err := manifest.Load(inst.Root, other, inst.Names.Secret); err == nil && o.Persona != nil && *o.Persona == *a.Persona {
			r.Shared = append(r.Shared, other)
		}
	}
	if !r.Editable {
		return r, nil
	}
	b, err := os.ReadFile(filepath.Join(inst.Root, r.Path, "persona.md"))
	if err != nil {
		if os.IsNotExist(err) {
			return r, nil
		}
		return nil, err
	}
	r.Hash, r.Look = config.Hash(b), Read(string(b))
	return r, nil
}

// SetAgent writes agent id's look. An agent with no persona gets personas/<id>/persona.md and its
// agent.yaml points at it; otherwise the persona.md must still hash to ifHash.
func SetAgent(inst *instance.Instance, id string, l Look, ifHash string) (*AgentLook, error) {
	if err := l.Validate(); err != nil {
		return nil, refuse("invalid", "%v", err)
	}
	cur, err := ShowAgent(inst, id)
	if err != nil {
		return nil, err
	}
	if !cur.Editable {
		return nil, refuse("readonly", "%s: %s", id, cur.Reason)
	}
	if cur.Path == "" {
		return create(inst, id, l)
	}
	p := filepath.Join(inst.Root, cur.Path, "persona.md")
	if cur.Hash != ifHash {
		return nil, refuse("conflict", "%s/persona.md changed on disk since it was loaded; reload it and edit again", cur.Path)
	}
	old := ""
	if b, err := os.ReadFile(p); err == nil {
		old = string(b)
	}
	var text string
	if old == "" {
		a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			return nil, err
		}
		text, err = New(a.Name, id, l)
		if err != nil {
			return nil, refuse("invalid", "%v", err)
		}
	} else if text, err = Render(old, l); err != nil {
		return nil, refuse("invalid", "%v", err)
	}
	if err := writeAtomic(p, text); err != nil {
		return nil, err
	}
	return ShowAgent(inst, id)
}

func create(inst *instance.Instance, id string, l Look) (*AgentLook, error) {
	rel := "personas/" + id
	dir := filepath.Join(inst.Root, rel)
	if _, err := os.Stat(dir); err == nil {
		return nil, refuse("conflict", "%s already exists and is not %s's; point its agent.yaml persona at it to reuse it", rel, id)
	}
	a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	text, err := New(a.Name, id, l)
	if err != nil {
		return nil, refuse("invalid", "%v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "persona.md"), []byte(text), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	yamlRel := "agents/" + id + "/agent.yaml"
	f, err := config.Show(inst, yamlRel)
	if err == nil {
		patch, _ := json.Marshal(map[string]any{"persona": map[string]any{"path": rel}})
		_, err = config.Apply(inst, yamlRel, patch, f.Hash)
	}
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return ShowAgent(inst, id)
}

func writeAtomic(p, text string) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
