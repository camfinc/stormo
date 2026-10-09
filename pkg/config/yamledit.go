package config

import (
	"bytes"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Editing YAML that people wrote by hand: a JSON merge patch (RFC 7386) is applied to the file's
// node tree, so comments and key order stay, and the result is re-encoded with the original
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

// encode writes doc back as YAML in the style of original: lines that differ from the original
// only in the spacing before a comment are the original lines, and the original's blank lines
// come back where their neighbours still are.
func encode(doc *yaml.Node, original []byte) ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return restore(original, b.Bytes()), nil
}

var beforeComment = regexp.MustCompile(`(\S)[ \t]{2,}#`)

func norm(line string) string {
	return beforeComment.ReplaceAllString(strings.TrimRight(line, " \t"), "$1 #")
}

// restore walks a longest common subsequence of the two files' lines (compared by norm): common
// lines are taken from the original, lines only in the new file are kept, and lines only in the
// original are dropped, except blank ones, which the encoder never writes. A blank line is put back
// once: two blanks meet only when they were adjacent in the original. A top-level key the original
// did not have starts a section of its own when the original separates sections with blank lines.
func restore(original, encoded []byte) []byte {
	o := strings.Split(strings.TrimSuffix(string(original), "\n"), "\n")
	n := strings.Split(strings.TrimSuffix(string(encoded), "\n"), "\n")
	on, nn := make([]string, len(o)), make([]string, len(n))
	for i := range o {
		on[i] = norm(o[i])
	}
	for j := range n {
		nn[j] = norm(n[j])
	}
	// lcs[i][j]: common lines of o[i:] and n[j:].
	lcs := make([][]int, len(o)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(n)+1)
	}
	for i := len(o) - 1; i >= 0; i-- {
		for j := len(n) - 1; j >= 0; j-- {
			if on[i] == nn[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	topKeys, sections := map[string]bool{}, false
	for _, l := range o {
		if k, ok := topKey(l); ok {
			topKeys[k] = true
		}
		sections = sections || strings.TrimSpace(l) == ""
	}
	out := []string{}
	lastBlank := -2 // original index of the last blank line written
	blank := func(i int) {
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" && lastBlank != i-1 {
			return
		}
		out, lastBlank = append(out, o[i]), i
	}
	i, j := 0, 0
	for i < len(o) || j < len(n) {
		switch {
		case i < len(o) && j < len(n) && on[i] == nn[j] && lcs[i][j] == lcs[i+1][j+1]+1:
			if strings.TrimSpace(o[i]) == "" {
				blank(i)
			} else {
				out = append(out, o[i])
			}
			i, j = i+1, j+1
		case j < len(n) && (i == len(o) || lcs[i][j+1] >= lcs[i+1][j]):
			if k, ok := topKey(n[j]); ok && !topKeys[k] && sections && len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
				out = append(out, "")
			}
			out = append(out, n[j])
			j++
		default:
			if strings.TrimSpace(o[i]) == "" && len(out) > 0 {
				blank(i)
			}
			i++
		}
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

var topKeyRe = regexp.MustCompile(`^([A-Za-z_][\w.-]*):`)

func topKey(line string) (string, bool) {
	m := topKeyRe.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}
