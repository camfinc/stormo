package config

import "go.yaml.in/yaml/v3"

// Editing YAML that people wrote by hand: a JSON merge patch (RFC 7386) is applied to the file's
// node tree, so comments and key order stay, and the result is re-encoded (yamlfmt) with the original
// file's blank lines and comment alignment put back (yaml.v3 drops both).

// applyPatch applies a merge patch object to a mapping node: null deletes a key, an object merges
// into an object, anything else replaces the value. A replaced value keeps what it can of the old
// one (see replaceNode).
func applyPatch(m, patch *yaml.Node) {
	for i := 0; i+1 < len(patch.Content); i += 2 {
		k, v := patch.Content[i].Value, patch.Content[i+1]
		at := keyIndex(m, k)
		switch {
		case isNull(v):
			if at >= 0 {
				m.Content = append(m.Content[:at], m.Content[at+2:]...)
			}
		case at < 0:
			m.Content = append(m.Content, plain(patch.Content[i]), plain(v))
		case v.Kind == yaml.MappingNode && m.Content[at+1].Kind == yaml.MappingNode:
			applyPatch(m.Content[at+1], v)
		default:
			m.Content[at+1] = replaceNode(m.Content[at+1], v)
		}
	}
}

// replaceNode is the new value nw in place of old, reusing old's nodes where nothing changed so their
// comments and styles stay: an equal scalar is the old node; a changed scalar keeps the old one's
// comments; a mapping keeps its existing keys in their order (keys nw lacks are dropped); a
// sequence reuses an equal scalar item wherever it now sits and merges mappings by position.
func replaceNode(old, nw *yaml.Node) *yaml.Node {
	nw = plain(nw)
	switch {
	case old.Kind == yaml.ScalarNode && nw.Kind == yaml.ScalarNode:
		if sameScalar(old, nw) {
			return old
		}
	case old.Kind == yaml.MappingNode && nw.Kind == yaml.MappingNode:
		out := &yaml.Node{Kind: yaml.MappingNode, Tag: old.Tag, Style: old.Style}
		for i := 0; i+1 < len(old.Content); i += 2 {
			if at := keyIndex(nw, old.Content[i].Value); at >= 0 {
				out.Content = append(out.Content, old.Content[i], replaceNode(old.Content[i+1], nw.Content[at+1]))
			}
		}
		for i := 0; i+1 < len(nw.Content); i += 2 {
			if keyIndex(old, nw.Content[i].Value) < 0 {
				out.Content = append(out.Content, nw.Content[i], nw.Content[i+1])
			}
		}
		nw = out
	case old.Kind == yaml.SequenceNode && nw.Kind == yaml.SequenceNode:
		out := &yaml.Node{Kind: yaml.SequenceNode, Tag: old.Tag, Style: old.Style}
		used := make([]bool, len(old.Content))
		for j, item := range nw.Content {
			reused := item
			if item.Kind == yaml.ScalarNode {
				for i, o := range old.Content {
					if !used[i] && o.Kind == yaml.ScalarNode && sameScalar(o, item) {
						reused, used[i] = o, true
						break
					}
				}
			} else if j < len(old.Content) && !used[j] && old.Content[j].Kind == item.Kind {
				reused, used[j] = replaceNode(old.Content[j], item), true
			}
			out.Content = append(out.Content, reused)
		}
		nw = out
	}
	nw.HeadComment, nw.LineComment, nw.FootComment = old.HeadComment, old.LineComment, old.FootComment
	return nw
}

func keyIndex(m *yaml.Node, key string) int {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" }

func sameScalar(a, b *yaml.Node) bool { return a.Value == b.Value && a.ShortTag() == b.ShortTag() }

// plain is a patch value as block YAML: JSON's quoting and flow style cleared (the encoder still
// quotes a string that would read as another type) and, inside new objects, nulls dropped.
func plain(n *yaml.Node) *yaml.Node {
	c := *n
	c.Style = 0
	c.Content = nil
	for i := 0; i < len(n.Content); i++ {
		if n.Kind == yaml.MappingNode && i+1 < len(n.Content) && isNull(n.Content[i+1]) {
			i++
			continue
		}
		c.Content = append(c.Content, plain(n.Content[i]))
	}
	return &c
}
