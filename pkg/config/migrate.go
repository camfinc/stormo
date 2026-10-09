package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/yamlfmt"
)

// MigrateReport is what MigrateAgent did to one agent.
type MigrateReport struct {
	Agent   string   `json:"agent"`
	From    int      `json:"from"`
	To      int      `json:"to"`
	Changes []string `json:"changes"`
	// Data is what happened to the agent's data (ops.MigrateAgentData): moved (into
	// agents/<id>/data), in-place (already there), running (left until the agent is stopped).
	Data string `json:"data"`
}

// MigrateAgent brings agent id's definition to agent.yaml format 1 (docs/agent-standard.md):
// agent.yaml gains format, model, memory, limits and schedules (edited on its node tree, so
// comments and layout stay), the engine's format-0 files move into engine/<kind>/, and skills and
// scripts say $AGENT_HOME. Safe while the agent runs: it takes effect on the next start. Its data
// moves separately (ops.MigrateAgentData).
func MigrateAgent(inst *instance.Instance, id string) (*MigrateReport, error) {
	a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	r := &MigrateReport{Agent: id, From: a.Format, To: manifest.Format, Changes: []string{}}
	dir := manifest.AgentDir(inst.Root, id)
	eng, err := engines.Get(a.Engine.Kind, inst)
	if err != nil {
		return nil, err
	}
	mig := &engine.Migration{}
	if fz, ok := eng.(engine.FormatZero); ok {
		if mig, err = fz.MigrateFormatZero(dir); err != nil {
			return nil, fmt.Errorf("agents/%s: %w", id, err)
		}
	}

	// agent.yaml
	yamlPath := filepath.Join(dir, "agent.yaml")
	old, err := os.ReadFile(yamlPath)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(old, &doc); err != nil {
		return nil, err
	}
	m := doc.Content[0]
	if a.Format < manifest.Format {
		upgradeManifest(m, mig)
		r.Changes = append(r.Changes, fmt.Sprintf("agent.yaml: format %d → %d", a.Format, manifest.Format))
	}
	if mig.Schedules != nil && keyIndex(m, "schedules") < 0 {
		insertBefore(m, []string{"state", "learning", "memory", "limits", "deploy"}, "schedules", schedulesNode(mig.Schedules), "")
		r.Changes = append(r.Changes, fmt.Sprintf("agent.yaml: %d schedules (were the engine's own file)", len(mig.Schedules)))
	}
	body, err := yamlfmt.Encode(&doc, old)
	if err != nil {
		return nil, err
	}
	// Validate before anything is written: the new manifest must load as format 1.
	moved, err := manifest.Parse(inst.Root, id, inst.Names.Secret, body)
	if err != nil {
		return nil, fmt.Errorf("the migrated agent.yaml would not load: %w", err)
	}
	if len(moved.Legacy) > 0 {
		return nil, fmt.Errorf("agents/%s: %s still in agent.yaml after the migration", id, strings.Join(moved.Legacy, ", "))
	}

	// Skills and scripts: $AGENT_HOME.
	rewrites := map[string][]byte{}
	for _, sub := range []string{"skills", "scripts"} {
		_ = filepath.WalkDir(filepath.Join(dir, sub), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil || !hermesHome.Match(b) {
				return nil
			}
			rewrites[p] = hermesHome.ReplaceAll(b, []byte("AGENT_HOME"))
			return nil
		})
	}

	// Write.
	for rel, b := range mig.Write {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, err
		}
		r.Changes = append(r.Changes, "wrote "+rel)
	}
	for from, to := range mig.Rename {
		p := filepath.Join(dir, filepath.FromSlash(to))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.Rename(filepath.Join(dir, filepath.FromSlash(from)), p); err != nil {
			return nil, err
		}
		r.Changes = append(r.Changes, "moved "+from+"/ → "+to+"/")
	}
	for p, b := range rewrites {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, err
		}
	}
	if len(rewrites) > 0 {
		r.Changes = append(r.Changes, fmt.Sprintf("$HERMES_HOME → $AGENT_HOME in %d skill and script files", len(rewrites)))
	}
	if string(body) != string(old) {
		if err := replace(yamlPath, body); err != nil {
			return nil, err
		}
	}
	for _, rel := range mig.Remove {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			if entries, _ := os.ReadDir(p); len(entries) > 0 {
				r.Changes = append(r.Changes, "kept "+rel+"/: it still holds files the migration does not know")
				continue
			}
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		r.Changes = append(r.Changes, "removed "+rel)
	}

	return r, nil
}

var hermesHome = regexp.MustCompile(`\bHERMES_HOME\b`)

// upgradeManifest moves format 0's keys to format 1's: engine.model/provider/local → model:,
// learning.*_char_limit → memory:, the engine's lifted settings → limits:. Key nodes move with
// their comments.
func upgradeManifest(m *yaml.Node, mig *engine.Migration) {
	if i := keyIndex(m, "format"); i >= 0 {
		m.Content[i+1] = scalar(strconv.Itoa(manifest.Format), "!!int")
	} else {
		// The file's opening comment stays on top: it moves from the first key to format.
		k := scalar("format", "!!str")
		if len(m.Content) > 0 {
			k.HeadComment, m.Content[0].HeadComment = m.Content[0].HeadComment, ""
		}
		m.Content = append([]*yaml.Node{k, scalar(strconv.Itoa(manifest.Format), "!!int")}, m.Content...)
	}
	if e := value(m, "engine"); e != nil && e.Kind == yaml.MappingNode {
		model := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		take(e, "model", "name", model)
		take(e, "provider", "provider", model)
		if i := keyIndex(e, "local"); i >= 0 {
			if l := e.Content[i+1]; l.Kind == yaml.MappingNode {
				if j := keyIndex(l, "model"); j >= 0 {
					l.Content[j].Value = "name"
				}
			}
			take(e, "local", "local", model)
		}
		if len(model.Content) > 0 {
			insertAfter(m, "engine", "model", model)
		}
	}
	if l := value(m, "learning"); l != nil && l.Kind == yaml.MappingNode {
		memory := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		take(l, "memory_char_limit", "agent", memory)
		take(l, "user_char_limit", "user", memory)
		if len(memory.Content) > 0 {
			insertBefore(m, []string{"learning"}, "memory", memory, "")
		}
		if len(l.Content) == 0 {
			i := keyIndex(m, "learning")
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
		}
	}
	limits := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, kv := range []struct {
		key string
		v   any
	}{{"turns", mig.Limits.Turns}, {"reasoning", mig.Limits.Reasoning}, {"command_timeout", mig.Limits.CommandTimeout}, {"script_timeout", mig.Limits.ScriptTimeout}} {
		s := fmt.Sprint(kv.v)
		if s == "" || s == "0" {
			continue
		}
		k := scalar(kv.key, "!!str")
		k.HeadComment = commentLines(mig.Comments[kv.key])
		tag := "!!int"
		if kv.key == "reasoning" {
			tag = "!!str"
		}
		limits.Content = append(limits.Content, k, scalar(s, tag))
	}
	if len(limits.Content) > 0 && keyIndex(m, "limits") < 0 {
		insertBefore(m, []string{"deploy"}, "limits", limits, "")
	}
}

func commentLines(c string) string {
	if c == "" {
		return ""
	}
	lines := strings.Split(c, "\n")
	for i, l := range lines {
		if l = strings.TrimSpace(l); !strings.HasPrefix(l, "#") {
			l = "# " + l
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

func scalar(v, tag string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: v} }

func value(m *yaml.Node, key string) *yaml.Node {
	if i := keyIndex(m, key); i >= 0 {
		return m.Content[i+1]
	}
	return nil
}

// take moves key (renamed to as) from m to the end of dst, with its comments.
func take(m *yaml.Node, key, as string, dst *yaml.Node) {
	i := keyIndex(m, key)
	if i < 0 {
		return
	}
	k, v := m.Content[i], m.Content[i+1]
	k.Value = as
	m.Content = append(m.Content[:i], m.Content[i+2:]...)
	dst.Content = append(dst.Content, k, v)
}

func insertAfter(m *yaml.Node, after, key string, v *yaml.Node) {
	i := keyIndex(m, after)
	if i < 0 {
		m.Content = append(m.Content, scalar(key, "!!str"), v)
		return
	}
	rest := append([]*yaml.Node{scalar(key, "!!str"), v}, m.Content[i+2:]...)
	m.Content = append(m.Content[:i+2], rest...)
}

// insertBefore puts key before the first of before that m has, else at the end.
func insertBefore(m *yaml.Node, before []string, key string, v *yaml.Node, comment string) {
	k := scalar(key, "!!str")
	k.HeadComment = comment
	for _, b := range before {
		if i := keyIndex(m, b); i >= 0 {
			// The section's head comment stays with the section it described.
			rest := append([]*yaml.Node{k, v}, m.Content[i:]...)
			m.Content = append(m.Content[:i:i], rest...)
			return
		}
	}
	m.Content = append(m.Content, k, v)
}

// schedulesNode is schedules: as people write it: defaults left out, prompts as literal blocks,
// lists in flow style.
func schedulesNode(list []manifest.Schedule) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, s := range list {
		j := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		add := func(k string, v *yaml.Node) { j.Content = append(j.Content, scalar(k, "!!str"), v) }
		str := func(k, v string) {
			if v == "" {
				return
			}
			n := scalar(v, "!!str")
			if strings.Contains(v, "\n") {
				n.Style = yaml.LiteralStyle
			}
			add(k, n)
		}
		list := func(k string, l []string) {
			if len(l) == 0 {
				return
			}
			n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
			for _, x := range l {
				n.Content = append(n.Content, scalar(x, "!!str"))
			}
			add(k, n)
		}
		str("id", s.ID)
		str("name", s.Name)
		str("every", s.Every)
		str("cron", s.Cron)
		if !s.Enabled {
			add("enabled", scalar("false", "!!bool"))
		}
		str("note", s.Note)
		str("prompt", s.Prompt)
		list("skills", s.Skills)
		str("script", s.Script)
		if !s.Agent {
			add("agent", scalar("false", "!!bool"))
		}
		str("monitor", s.Monitor)
		str("model", s.Model)
		str("provider", s.Provider)
		list("tools", s.Tools)
		str("deliver", s.Deliver)
		str("on_failure", s.OnFailure)
		if s.Times > 0 {
			add("times", scalar(strconv.Itoa(s.Times), "!!int"))
		}
		seq.Content = append(seq.Content, j)
	}
	return seq
}
