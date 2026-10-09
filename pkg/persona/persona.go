// Package persona reads and writes an agent's look: personas/<slug>/persona.md, Markdown whose YAML
// frontmatter carries the office's `sprite:` (the character) and `desk:` (what sits on its desk),
// and whose `## Avatar lock` section describes the face a portrait is baked from. A persona is a
// look only; behaviour lives in the agent's SOUL.md.
//
// Render changes only those three parts: other frontmatter keys, their comments and the rest of
// the Markdown stay as the instance wrote them.
package persona

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/pkg/yamlfmt"
)

// Look is what an editor changes. Nil Sprite or Desk means "none": the office falls back to colours
// seeded by the agent id and to its unit's desk.
type Look struct {
	// Description is the `## Avatar lock` text: who the portrait shows, for a later bake.
	Description string  `json:"description"`
	Sprite      *Sprite `json:"sprite,omitempty"`
	Desk        *Desk   `json:"desk,omitempty"`
}

// Sprite is the office character. Colours are #rrggbb.
type Sprite struct {
	Skin      string `json:"skin,omitempty" yaml:"skin,omitempty"`
	Hair      string `json:"hair,omitempty" yaml:"hair,omitempty"`
	HairStyle string `json:"hair_style,omitempty" yaml:"hair_style,omitempty"`
	Shirt     string `json:"shirt,omitempty" yaml:"shirt,omitempty"`
	Pants     string `json:"pants,omitempty" yaml:"pants,omitempty"`
	Accessory string `json:"accessory,omitempty" yaml:"accessory,omitempty"`
}

// Desk is what sits on the agent's desk: one app per monitor (1 or 2), up to MaxProps props in slot
// order and one floor item beside it.
type Desk struct {
	Apps    []string `json:"apps,omitempty"`
	Screens int      `json:"screens,omitempty"`
	Props   []string `json:"props,omitempty"`
	Side    string   `json:"side,omitempty"`
}

// The short lists the office draws (docs/instances.md, Personas). Desk names are free-form
// identifiers: a UI ignores the ones it does not draw.
var (
	HairStyles  = []string{"short", "fade", "long", "updo", "curly", "bald"}
	Accessories = []string{"none", "headset", "glasses", "cap", "earrings"}
)

const MaxProps = 6

var (
	fmRe    = regexp.MustCompile(`^---\r?\n((?s:.*?))\r?\n---[ \t]*(?:\r?\n|$)`)
	colorRe = regexp.MustCompile(`^#[0-9a-f]{6}$`)
	nameRe  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	lockRe  = regexp.MustCompile(`(?m)^##[ \t]+Avatar lock[ \t]*$`)
	headRe  = regexp.MustCompile(`(?m)^#{1,2}[ \t]`)
)

// Validate refuses a look the office could not draw as written.
func (l Look) Validate() error {
	if s := l.Sprite; s != nil {
		for k, v := range map[string]string{"skin": s.Skin, "hair": s.Hair, "shirt": s.Shirt, "pants": s.Pants} {
			if v != "" && !colorRe.MatchString(v) {
				return fmt.Errorf("sprite %s %q: a colour is #rrggbb in lowercase", k, v)
			}
		}
		if s.HairStyle != "" && !slices.Contains(HairStyles, s.HairStyle) {
			return fmt.Errorf("sprite hair_style %q: one of %s", s.HairStyle, strings.Join(HairStyles, ", "))
		}
		if s.Accessory != "" && !slices.Contains(Accessories, s.Accessory) {
			return fmt.Errorf("sprite accessory %q: one of %s", s.Accessory, strings.Join(Accessories, ", "))
		}
	}
	if d := l.Desk; d != nil {
		if d.Screens < 0 || d.Screens > 2 || len(d.Apps) > 2 {
			return fmt.Errorf("desk: 1 or 2 screens, one app each")
		}
		if len(d.Props) > MaxProps {
			return fmt.Errorf("desk: up to %d props", MaxProps)
		}
		for _, n := range append(append([]string{d.Side}, d.Apps...), d.Props...) {
			if n != "" && !nameRe.MatchString(n) {
				return fmt.Errorf("desk: %q is not a name (lowercase letters, digits, hyphens)", n)
			}
		}
	}
	return nil
}

// Read is the look in a persona.md. What does not parse is left out rather than failing.
func Read(md string) Look {
	var l Look
	if fm := frontmatter(md); fm != nil {
		var raw struct {
			Sprite *Sprite         `yaml:"sprite"`
			Desk   *map[string]any `yaml:"desk"`
		}
		if yaml.Unmarshal([]byte(fm[1]), &raw) == nil {
			l.Sprite = raw.Sprite
			if raw.Desk != nil {
				l.Desk = readDesk(*raw.Desk)
			}
		}
	}
	if loc := lockRe.FindStringIndex(md); loc != nil {
		body := md[loc[1]:]
		if next := headRe.FindStringIndex(body); next != nil {
			body = body[:next[0]]
		}
		l.Description = strings.TrimSpace(body)
	}
	return l
}

func readDesk(d map[string]any) *Desk {
	desk := &Desk{}
	str := func(v any) string { s, _ := v.(string); return s }
	switch a := d["app"].(type) {
	case string:
		desk.Apps = []string{a}
	case []any:
		for _, x := range a {
			if s := str(x); s != "" {
				desk.Apps = append(desk.Apps, s)
			}
		}
	}
	if s, ok := d["screens"].(int); ok {
		desk.Screens = s
	}
	if props, ok := d["props"].([]any); ok {
		for _, x := range props {
			if s := str(x); s != "" {
				desk.Props = append(desk.Props, s)
			}
		}
	}
	desk.Side = str(d["side"])
	return desk
}

func frontmatter(md string) []string { return fmRe.FindStringSubmatch(md) }

// New is a persona.md for a new look, in the instance standard's shape.
func New(name, agentID string, l Look) (string, error) {
	return Render(fmt.Sprintf("# %s\n\nLook only (behaviour lives in agents/%s/SOUL.md).\n", name, agentID), l)
}

// Render is md with l's sprite, desk and Avatar lock; everything else in md stays.
func Render(md string, l Look) (string, error) {
	if err := l.Validate(); err != nil {
		return "", err
	}
	fmText, body := "", md
	if m := frontmatter(md); m != nil {
		fmText, body = m[1], md[len(m[0]):]
	}
	fm, err := renderFrontmatter(fmText, l)
	if err != nil {
		return "", err
	}
	body = renderLock(body, l.Description)
	if strings.TrimSpace(fm) == "" {
		return body, nil
	}
	return "---\n" + strings.TrimRight(fm, "\n") + "\n---\n\n" + strings.TrimLeft(body, "\n"), nil
}

func renderFrontmatter(text string, l Look) (string, error) {
	var doc yaml.Node
	if strings.TrimSpace(text) != "" {
		if err := yaml.Unmarshal([]byte(text), &doc); err != nil || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
			return "", fmt.Errorf("persona.md frontmatter does not parse as a YAML mapping; fix it as text first")
		}
	} else {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	root := doc.Content[0]
	if err := setKey(root, "desk", deskNode(l.Desk)); err != nil {
		return "", err
	}
	if err := setKey(root, "sprite", spriteNode(l.Sprite)); err != nil {
		return "", err
	}
	if len(root.Content) == 0 {
		return "", nil
	}
	b, err := yamlfmt.Encode(&doc, []byte(text))
	return string(b), err
}

// setKey replaces key's value, appends it when absent, removes it when v is nil.
func setKey(m *yaml.Node, key string, v *yaml.Node) error {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			switch {
			case v == nil:
				m.Content = append(m.Content[:i], m.Content[i+2:]...)
			case m.Content[i+1].Kind == yaml.MappingNode:
				merge(m.Content[i+1], v)
			default:
				m.Content[i+1] = v
			}
			return nil
		}
	}
	if v != nil {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
	}
	return nil
}

// merge makes mapping m say what n says, key by key: a value that did not change keeps its node
// (and comments), a changed one keeps its line comment, keys n lacks go, new ones are appended.
func merge(m, n *yaml.Node) {
	want := map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		want[n.Content[i].Value] = n.Content[i+1]
	}
	kept := m.Content[:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		nv, ok := want[k.Value]
		if !ok {
			continue
		}
		delete(want, k.Value)
		if !same(v, nv) {
			nv.LineComment = v.LineComment
			v = nv
		}
		kept = append(kept, k, v)
	}
	m.Content = kept
	for i := 0; i+1 < len(n.Content); i += 2 {
		if _, ok := want[n.Content[i].Value]; ok {
			m.Content = append(m.Content, n.Content[i], n.Content[i+1])
		}
	}
}

func same(a, b *yaml.Node) bool {
	var x, y any
	return a.Decode(&x) == nil && b.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

func spriteNode(s *Sprite) *yaml.Node {
	if s == nil || *s == (Sprite{}) {
		return nil
	}
	n := mapping()
	for _, kv := range [][2]string{{"skin", s.Skin}, {"hair", s.Hair}, {"hair_style", s.HairStyle}, {"shirt", s.Shirt}, {"pants", s.Pants}, {"accessory", s.Accessory}} {
		if kv[1] == "" {
			continue
		}
		v := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv[1]}
		if strings.HasPrefix(kv[1], "#") {
			v.Style = yaml.DoubleQuotedStyle
		}
		n.Content = append(n.Content, scalar(kv[0]), v)
	}
	return n
}

func deskNode(d *Desk) *yaml.Node {
	if d == nil || (len(d.Apps) == 0 && d.Screens == 0 && len(d.Props) == 0 && d.Side == "") {
		return nil
	}
	n := mapping()
	switch len(d.Apps) {
	case 0:
	case 1:
		n.Content = append(n.Content, scalar("app"), scalar(d.Apps[0]))
	default:
		n.Content = append(n.Content, scalar("app"), flow(d.Apps))
	}
	if d.Screens > 0 {
		n.Content = append(n.Content, scalar("screens"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(d.Screens)})
	}
	if len(d.Props) > 0 {
		n.Content = append(n.Content, scalar("props"), flow(d.Props))
	}
	if d.Side != "" {
		n.Content = append(n.Content, scalar("side"), scalar(d.Side))
	}
	return n
}

func mapping() *yaml.Node        { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }
func scalar(s string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s} }

func flow(xs []string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
	for _, x := range xs {
		n.Content = append(n.Content, scalar(x))
	}
	return n
}

// renderLock puts desc under `## Avatar lock`, replacing that section's text or adding the section
// at the end. An empty desc empties an existing section and adds none.
func renderLock(body, desc string) string {
	desc = strings.TrimSpace(desc)
	loc := lockRe.FindStringIndex(body)
	if loc == nil {
		if desc == "" {
			return body
		}
		return strings.TrimRight(body, "\n") + "\n\n## Avatar lock\n\n" + desc + "\n"
	}
	rest := body[loc[1]:]
	tail := ""
	if next := headRe.FindStringIndex(rest); next != nil {
		tail = "\n" + rest[next[0]:]
	}
	section := "\n"
	if desc != "" {
		section = "\n\n" + desc + "\n"
	}
	return body[:loc[1]] + section + tail
}
