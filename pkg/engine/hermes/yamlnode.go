package hermes

import (
	"bytes"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Editing a YAML document as a node tree keeps the ported base config's key order and comments,
// so the generated config.yaml still reads like the file it came from.

func findKey(m *yaml.Node, key string) (int, *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return -1, nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i, m.Content[i+1]
		}
	}
	return -1, nil
}

func getPath(root *yaml.Node, path string) *yaml.Node {
	n := root
	for _, k := range strings.Split(path, ".") {
		if _, n = findKey(n, k); n == nil {
			return nil
		}
	}
	return n
}

func addKey(m *yaml.Node, key string, v *yaml.Node) {
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}

func deleteKey(m *yaml.Node, key string) {
	if i, _ := findKey(m, key); i >= 0 {
		m.Content = append(m.Content[:i], m.Content[i+2:]...)
	}
}

func toNode(v any) (*yaml.Node, error) {
	if n, ok := v.(*yaml.Node); ok {
		return n, nil
	}
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	return &n, nil
}

// setNode sets path (dot-separated) to v, creating mappings on the way (`node[k] ??= {}`).
func setNode(root *yaml.Node, path string, v *yaml.Node) error {
	keys := strings.Split(path, ".")
	n := root
	for _, k := range keys[:len(keys)-1] {
		_, child := findKey(n, k)
		if child == nil || child.Tag == "!!null" {
			fresh := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if child == nil {
				addKey(n, k, fresh)
			} else {
				*child = *fresh
			}
			child = fresh
			if i, c := findKey(n, k); i >= 0 {
				child = c
			}
		}
		if child.Kind != yaml.MappingNode {
			return fmt.Errorf("config: %s is not a mapping", k)
		}
		n = child
	}
	last := keys[len(keys)-1]
	if i, _ := findKey(n, last); i >= 0 {
		v.HeadComment, v.LineComment = n.Content[i+1].HeadComment, n.Content[i+1].LineComment
		n.Content[i+1] = v
	} else {
		addKey(n, last, v)
	}
	return nil
}

func setPath(root *yaml.Node, path string, v any) error {
	n, err := toNode(v)
	if err != nil {
		return err
	}
	return setNode(root, path, n)
}

// yamlMap builds an ordered mapping from key, value pairs.
func yamlMap(kv ...any) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for i := 0; i+1 < len(kv); i += 2 {
		v, err := toNode(kv[i+1])
		if err != nil {
			panic(err)
		}
		addKey(m, kv[i].(string), v)
	}
	return m
}

func encodeYAML(n *yaml.Node) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
