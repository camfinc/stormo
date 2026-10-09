// Package yamlfmt re-encodes YAML that people wrote by hand without losing its look: yaml.v3 keeps
// comments and key order on the node tree but drops blank lines and comment alignment; Encode puts
// those back wherever the content did not change.
package yamlfmt

import (
	"bytes"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Encode writes doc back as YAML in the style of original: lines that differ from the original
// only in the spacing before a comment are the original lines, and the original's blank lines
// come back where their neighbours still are.
func Encode(doc *yaml.Node, original []byte) ([]byte, error) {
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

// norm is what a line means, for matching an encoded line with the original it came from: the
// spacing before a comment does not count, and a one-line `key: value` (or `- value`) is compared
// by its key, decoded value and comment, so a scalar the encoder only re-quoted (yaml.v3 escapes
// emoji in double quotes) still matches its original.
func norm(line string) string {
	trimmed := strings.TrimRight(line, " \t")
	body := strings.TrimLeft(trimmed, " ")
	indent := trimmed[:len(trimmed)-len(body)]
	if body != "" && !strings.HasPrefix(body, "#") {
		var doc yaml.Node
		if yaml.Unmarshal([]byte(body), &doc) == nil && len(doc.Content) == 1 {
			n := doc.Content[0]
			switch {
			case n.Kind == yaml.MappingNode && len(n.Content) == 2 && n.Content[1].Kind == yaml.ScalarNode && n.Content[1].Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0:
				k, v := n.Content[0], n.Content[1]
				return indent + "\x00k\x00" + k.Value + "\x00" + v.ShortTag() + "\x00" + v.Value + "\x00" + strings.TrimSpace(v.LineComment+k.LineComment)
			case n.Kind == yaml.SequenceNode && len(n.Content) == 1 && n.Content[0].Kind == yaml.ScalarNode && n.Content[0].Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0:
				v := n.Content[0]
				return indent + "\x00s\x00" + v.ShortTag() + "\x00" + v.Value + "\x00" + strings.TrimSpace(v.LineComment)
			}
		}
	}
	return beforeComment.ReplaceAllString(trimmed, "$1 #")
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
			if k, ok := topKey(n[j]); ok && !topKeys[k] && sections && len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" &&
				!strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "#") { // a comment above it is its own
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
