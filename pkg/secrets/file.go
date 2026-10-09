package secrets

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/camfinc/stormo/pkg/env"
	"go.yaml.in/yaml/v3"
)

// Layer is one set of NAME: value pairs, in file order. A name present with an empty (or null)
// value counts as declared but missing.
type Layer = env.Env

// Named is an ordered map of layers (units or agents).
type Named struct {
	keys []string
	m    map[string]*Layer
}

func NewNamed() *Named { return &Named{m: map[string]*Layer{}} }

func (n *Named) Get(k string) *Layer {
	if n == nil {
		return nil
	}
	return n.m[k]
}

// Ensure returns the layer for k, creating it at the end.
func (n *Named) Ensure(k string) *Layer {
	if l, ok := n.m[k]; ok {
		return l
	}
	n.keys = append(n.keys, k)
	n.m[k] = env.New()
	return n.m[k]
}

func (n *Named) Keys() []string {
	if n == nil {
		return nil
	}
	return append([]string{}, n.keys...)
}

// Layers is shared → units.<unit> → agents.<id>.
type Layers struct {
	Shared *Layer
	Units  *Named
	Agents *Named
}

// File is secrets.local.yaml: the aws layers plus a `local:` overlay of the same shape. An agent's
// own layers (agents.<id> and local.agents.<id>) can instead live in its folder,
// agents/<id>/data/secrets.yaml (AgentFile); Load merges them in and Save writes them back there.
type File struct {
	Layers
	Local *Layers
	// inFolder are the agents whose own layers live in their folder.
	inFolder map[string]bool
}

// AgentFile is where an agent's own secret values live in its folder.
func AgentFile(root, id string) string {
	return filepath.Join(root, "agents", id, "data", "secrets.yaml")
}

// InFolder reports whether agent id's own layers live in its folder.
func (f *File) InFolder(id string) bool { return f.inFolder[id] }

// MoveToFolder makes Save write agent id's own layers to its folder instead of the shared file.
func (f *File) MoveToFolder(id string) {
	if f.inFolder == nil {
		f.inFolder = map[string]bool{}
	}
	f.inFolder[id] = true
}

// agentDoc is AgentFile's shape: the agent's layer and its local overlay.
func agentFileBody(id string, own, local *Layer) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if own != nil {
		root.Content = append(root.Content, str("secrets"), layerNode(own))
	}
	if local != nil {
		root.Content = append(root.Content, str("local"), layerNode(local))
	}
	var b bytes.Buffer
	b.WriteString("# " + id + "'s own secret values (secrets: for every target, local: overrides for local runs).\n" +
		"# NEVER COMMIT: agents/" + id + "/data is gitignored. Shared and unit values stay in secrets.local.yaml.\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

// loadAgentFiles merges each agents/<id>/data/secrets.yaml under root into f.
func (f *File) loadAgentFiles(root string) error {
	paths, _ := filepath.Glob(filepath.Join(root, "agents", "*", "data", "secrets.yaml"))
	for _, p := range paths {
		id := filepath.Base(filepath.Dir(filepath.Dir(p)))
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(body, &doc); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if len(doc.Content) == 0 || doc.Content[0].Tag == "!!null" {
			f.MoveToFolder(id)
			continue
		}
		m := doc.Content[0]
		if m.Kind != yaml.MappingNode {
			return fmt.Errorf("%s: must be a map of secrets: and local:", p)
		}
		where := "agents/" + id + "/data/secrets.yaml"
		for i := 0; i+1 < len(m.Content); i += 2 {
			k := m.Content[i].Value
			if k != "secrets" && k != "local" {
				return fmt.Errorf("%s: unknown key %q (secrets, local)", where, k)
			}
		}
		if s := get(m, "secrets"); s != nil {
			l, err := layerFrom(s, where+" secrets")
			if err != nil {
				return err
			}
			if f.Agents.Get(id) != nil {
				return fmt.Errorf("%s's secret values are in both secrets.local.yaml (agents.%s) and %s; keep %s", id, id, where, where)
			}
			if f.Agents == nil {
				f.Agents = NewNamed()
			}
			f.Agents.Ensure(id).Merge(l)
		}
		if s := get(m, "local"); s != nil {
			l, err := layerFrom(s, where+" local")
			if err != nil {
				return err
			}
			if f.Local != nil && f.Local.Agents.Get(id) != nil {
				return fmt.Errorf("%s's local secret values are in both secrets.local.yaml (local.agents.%s) and %s; keep %s", id, id, where, where)
			}
			if f.Local == nil {
				f.Local = &Layers{}
			}
			if f.Local.Agents == nil {
				f.Local.Agents = NewNamed()
			}
			f.Local.Agents.Ensure(id).Merge(l)
		}
		f.MoveToFolder(id)
	}
	return nil
}

// Stamp changes whenever secrets.local.yaml or an agent's secrets file under root changes.
func Stamp(root string) string {
	paths, _ := filepath.Glob(filepath.Join(root, "agents", "*", "data", "secrets.yaml"))
	paths = append([]string{filepath.Join(root, "secrets.local.yaml")}, paths...)
	var b strings.Builder
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			fmt.Fprintf(&b, "%s:%d:%d;", p, info.ModTime().UnixNano(), info.Size())
		}
	}
	return b.String()
}

func layerFrom(n *yaml.Node, where string) (*Layer, error) {
	l := env.New()
	if n == nil || n.Tag == "!!null" {
		return l, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a map of NAME: value", where)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1]
		val := v.Value
		if v.Tag == "!!null" {
			val = ""
		}
		l.Set(n.Content[i].Value, val)
	}
	return l, nil
}

func namedFrom(n *yaml.Node, where string) (*Named, error) {
	out := NewNamed()
	if n == nil || n.Tag == "!!null" {
		return out, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a map", where)
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i].Value
		l, err := layerFrom(n.Content[i+1], where+"."+k)
		if err != nil {
			return nil, err
		}
		out.keys = append(out.keys, k)
		out.m[k] = l
	}
	return out, nil
}

func get(m *yaml.Node, key string) *yaml.Node {
	for i := 0; m != nil && i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func layersFrom(m *yaml.Node, prefix string) (Layers, error) {
	var ls Layers
	var err error
	if s := get(m, "shared"); s != nil {
		if ls.Shared, err = layerFrom(s, prefix+"shared"); err != nil {
			return ls, err
		}
	}
	if u := get(m, "units"); u != nil {
		if ls.Units, err = namedFrom(u, prefix+"units"); err != nil {
			return ls, err
		}
	}
	if a := get(m, "agents"); a != nil {
		if ls.Agents, err = namedFrom(a, prefix+"agents"); err != nil {
			return ls, err
		}
	}
	return ls, nil
}

// Parse reads a secrets file body.
func Parse(body []byte) (*File, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	f := &File{}
	if len(doc.Content) == 0 || doc.Content[0].Tag == "!!null" {
		return f, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("secrets file must be a map")
	}
	var err error
	if f.Layers, err = layersFrom(root, ""); err != nil {
		return nil, err
	}
	if l := get(root, "local"); l != nil && l.Tag != "!!null" {
		local, err := layersFrom(l, "local.")
		if err != nil {
			return nil, err
		}
		f.Local = &local
	}
	return f, nil
}

// Load reads path (an instance's secrets.local.yaml; a missing file is an empty one) and the
// instance's agent secrets files next to it.
func Load(path string) (*File, error) {
	f := &File{}
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil {
		if f, err = Parse(body); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := f.loadAgentFiles(filepath.Dir(path)); err != nil {
		return nil, err
	}
	return f, nil
}

func str(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v, Style: styleFor(v)}
}

func styleFor(v string) yaml.Style {
	if v == "" {
		return yaml.DoubleQuotedStyle
	}
	return 0
}

func layerNode(l *Layer) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, kv := range l.Pairs() {
		n.Content = append(n.Content, str(kv.Name), str(kv.Value))
	}
	return n
}

func namedNode(n *Named, skip map[string]bool) *yaml.Node {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, k := range n.keys {
		if skip[k] {
			continue
		}
		out.Content = append(out.Content, str(k), layerNode(n.m[k]))
	}
	return out
}

func layersNode(ls Layers, skip map[string]bool) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if ls.Shared != nil {
		n.Content = append(n.Content, str("shared"), layerNode(ls.Shared))
	}
	if ls.Units != nil {
		n.Content = append(n.Content, str("units"), namedNode(ls.Units, nil))
	}
	if ls.Agents != nil {
		if a := namedNode(ls.Agents, skip); len(a.Content) > 0 || len(skip) == 0 {
			n.Content = append(n.Content, str("agents"), a)
		}
	}
	return n
}

// Marshal renders secrets.local.yaml's body (without the header): every layer except the agent
// layers that live in their folder.
func (f *File) Marshal() ([]byte, error) {
	root := layersNode(f.Layers, f.inFolder)
	if f.Local != nil {
		root.Content = append(root.Content, str("local"), layersNode(*f.Local, f.inFolder))
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}
