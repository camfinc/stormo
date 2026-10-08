package secrets

import (
	"bytes"
	"fmt"
	"os"

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

// File is secrets.local.yaml: the aws layers plus a `local:` overlay of the same shape.
type File struct {
	Layers
	Local *Layers
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

// Load reads path; a missing file is an empty one.
func Load(path string) (*File, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &File{}, nil
	} else if err != nil {
		return nil, err
	}
	f, err := Parse(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
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

func namedNode(n *Named) *yaml.Node {
	out := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, k := range n.keys {
		out.Content = append(out.Content, str(k), layerNode(n.m[k]))
	}
	return out
}

func layersNode(ls Layers) *yaml.Node {
	n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if ls.Shared != nil {
		n.Content = append(n.Content, str("shared"), layerNode(ls.Shared))
	}
	if ls.Units != nil {
		n.Content = append(n.Content, str("units"), namedNode(ls.Units))
	}
	if ls.Agents != nil {
		n.Content = append(n.Content, str("agents"), namedNode(ls.Agents))
	}
	return n
}

// Marshal renders the file body (without the header).
func (f *File) Marshal() ([]byte, error) {
	root := layersNode(f.Layers)
	if f.Local != nil {
		root.Content = append(root.Content, str("local"), layersNode(*f.Local))
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}
