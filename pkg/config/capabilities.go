package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/yamlfmt"
)

// The instance's capabilities (stormo.yaml capabilities:) as an editor changes them: one capability
// at a time, by a JSON merge patch on its stanza, so the rest of the file (comments, order, blank
// lines) stays as people wrote it. A change is checked as the whole file before it is written:
// agents' sidecars read stormo.yaml live, so an invalid file never lands.

// DeclaredCapability is one capability as stormo.yaml says it (Spec, the stanza as written) and as it
// runs (Effective: with its template's defaults), with the agents granted it.
type DeclaredCapability struct {
	Name      string              `json:"name"`
	Spec      map[string]any      `json:"spec"`
	Effective instance.Capability `json:"effective"`
	Agents    []string            `json:"agents"`
}

// CapabilitiesFile is stormo.yaml's capabilities with the file's hash (for --if-hash).
type CapabilitiesFile struct {
	Path         string               `json:"path"`
	Hash         string               `json:"hash"`
	Capabilities []DeclaredCapability `json:"capabilities"`
}

// ShowCapabilities is stormo.yaml's capabilities.
func ShowCapabilities(inst *instance.Instance) (*CapabilitiesFile, error) {
	body, err := os.ReadFile(filepath.Join(inst.Root, instance.File))
	if err != nil {
		return nil, err
	}
	return capabilitiesOf(inst, body)
}

func capabilitiesOf(inst *instance.Instance, body []byte) (*CapabilitiesFile, error) {
	parsed, err := instance.Parse(inst.Root, body)
	if err != nil {
		return nil, refuse("invalid", "%v", err)
	}
	var raw struct {
		Capabilities map[string]map[string]any `yaml:"capabilities"`
	}
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, refuse("invalid", "%s: %v", instance.File, err)
	}
	grants := grantsOf(inst)
	out := &CapabilitiesFile{Path: instance.File, Hash: Hash(body), Capabilities: []DeclaredCapability{}}
	for _, c := range parsed.Capabilities {
		spec := raw.Capabilities[c.Name]
		if spec == nil {
			spec = map[string]any{}
		}
		agents := grants[c.Name]
		if agents == nil {
			agents = []string{}
		}
		c.Headers = nil // header templates can name secrets: not for editors' views
		out.Capabilities = append(out.Capabilities, DeclaredCapability{Name: c.Name, Spec: jsonSafe(spec), Effective: c, Agents: agents})
	}
	return out, nil
}

// jsonSafe turns what yaml.v3 decoded (map[string]any all the way down here) into what encoding/json
// takes: it already is, but a nested map[any]any would not be.
func jsonSafe(m map[string]any) map[string]any {
	b, err := json.Marshal(m)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}

// grantsOf is which agents are granted each capability (their agent.yaml capabilities:).
func grantsOf(inst *instance.Instance) map[string][]string {
	out := map[string][]string{}
	for _, id := range manifest.AgentIDs(inst.Root) {
		a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			continue
		}
		for _, c := range a.Capabilities {
			out[c] = append(out[c], id)
		}
	}
	for _, v := range out {
		sort.Strings(v)
	}
	return out
}

// SetCapability creates or changes one capability by a JSON merge patch on its stanza (null removes a
// setting). The file must not have changed since ifHash; the result must load and the capability must
// run on this engine (no unknown template).
func SetCapability(inst *instance.Instance, name string, patch []byte, ifHash string) (*CapabilitiesFile, error) {
	if !instance.CapabilityName.MatchString(name) {
		return nil, refuse("invalid", "%q: a capability's name is lowercase words joined by dots (media.render)", name)
	}
	var p yaml.Node
	if err := yaml.Unmarshal(patch, &p); err != nil || len(p.Content) != 1 || p.Content[0].Kind != yaml.MappingNode {
		return nil, refuse("usage", "the patch must be a JSON object (the capability's settings)")
	}
	return editCapabilities(inst, ifHash, func(caps *yaml.Node) error {
		at := keyIndex(caps, name)
		if at < 0 {
			caps.Content = append(caps.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
				&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", Style: yaml.FlowStyle})
			at = len(caps.Content) - 2
		}
		stanza := caps.Content[at+1]
		if stanza.Kind != yaml.MappingNode {
			return refuse("invalid", "capabilities.%s is not a mapping; fix it as text first", name)
		}
		applyPatch(stanza, p.Content[0])
		return nil
	}, func(parsed *instance.Instance) error {
		c, ok := parsed.CapabilityNamed(name)
		if !ok {
			return refuse("invalid", "capabilities.%s did not load", name)
		}
		if c.Unavailable != "" {
			return refuse("invalid", "capabilities.%s: %s", name, c.Unavailable)
		}
		return nil
	})
}

// RemoveCapability takes a capability out of stormo.yaml; refused while an agent is granted it.
func RemoveCapability(inst *instance.Instance, name, ifHash string) (*CapabilitiesFile, error) {
	if agents := grantsOf(inst)[name]; len(agents) > 0 {
		return nil, refuse("in_use", "%s is granted to %v: take it out of their capabilities first", name, agents)
	}
	return editCapabilities(inst, ifHash, func(caps *yaml.Node) error {
		at := keyIndex(caps, name)
		if at < 0 {
			return refuse("not_found", "no capability %s in %s", name, instance.File)
		}
		caps.Content = append(caps.Content[:at], caps.Content[at+2:]...)
		return nil
	}, nil)
}

// editCapabilities changes stormo.yaml's capabilities: mapping (made when missing) by edit, checks
// the whole file (Parse, then check), and writes it in one rename.
func editCapabilities(inst *instance.Instance, ifHash string, edit func(caps *yaml.Node) error, check func(*instance.Instance) error) (*CapabilitiesFile, error) {
	if ifHash == "" {
		return nil, refuse("usage", "%s: --if-hash is required (the hash `capability show` gave)", instance.File)
	}
	p := filepath.Join(inst.Root, instance.File)
	old, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if Hash(old) != ifHash {
		return nil, refuse("conflict", "%s changed on disk since it was loaded; reload it and edit again", instance.File)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(old, &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, refuse("invalid", "%s does not parse as a YAML mapping; fix it as text first", instance.File)
	}
	root := doc.Content[0]
	at := keyIndex(root, "capabilities")
	if at < 0 {
		root.Content = append(root.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "capabilities"},
			&yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
		at = len(root.Content) - 2
	}
	caps := root.Content[at+1]
	if isNull(caps) {
		caps.Kind, caps.Tag, caps.Value = yaml.MappingNode, "!!map", ""
	}
	if caps.Kind != yaml.MappingNode {
		return nil, refuse("invalid", "capabilities: is not a mapping; fix it as text first")
	}
	if err := edit(caps); err != nil {
		return nil, err
	}
	body, err := yamlfmt.Encode(&doc, old)
	if err != nil {
		return nil, err
	}
	parsed, err := instance.Parse(inst.Root, body)
	if err != nil {
		return nil, refuse("invalid", "%v", err)
	}
	if check != nil {
		if err := check(parsed); err != nil {
			return nil, err
		}
	}
	if !bytes.Equal(old, body) {
		if err := replace(p, body); err != nil {
			return nil, err
		}
	}
	inst.Capabilities = parsed.Capabilities
	return capabilitiesOf(inst, body)
}

// grantable is whether an agent.yaml may grant name: declared in stormo.yaml and able to run here.
func grantable(inst *instance.Instance, names []string) error {
	for _, n := range names {
		c, ok := inst.CapabilityNamed(n)
		if !ok {
			declared := []string{}
			for _, d := range inst.Capabilities {
				declared = append(declared, d.Name)
			}
			slices.Sort(declared)
			return refuse("invalid", "capability %q is not declared in %s (declared: %v)", n, instance.File, declared)
		}
		if c.Unavailable != "" {
			return refuse("invalid", "capability %q cannot run here: %s", n, c.Unavailable)
		}
	}
	return nil
}
