package hermes

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/yamlfmt"
	"go.yaml.in/yaml/v3"
)

// lifted are the Hermes config settings agent.yaml limits: now holds (ApplyOverrides writes them
// back), by config path.
var lifted = []struct{ path, limit string }{
	{"agent.max_turns", "turns"},
	{"agent.reasoning_effort", "reasoning"},
	{"terminal.timeout", "command_timeout"},
	{"cron.script_timeout_seconds", "script_timeout"},
}

// MigrateFormatZero: hermes/config.base.yaml becomes engine/hermes/config.yaml without the lifted
// settings (formatting and comments kept), plugins/ becomes engine/hermes/plugins/, and
// hermes/cron.jobs.json becomes agent.yaml schedules: (definitions only).
func (h *Hermes) MigrateFormatZero(dir string) (*engine.Migration, error) {
	m := &engine.Migration{Write: map[string][]byte{}, Rename: map[string]string{}, Comments: map[string]string{}}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
		return err == nil
	}
	const oldConfig, oldCron = "hermes/config.base.yaml", "hermes/cron.jobs.json"
	if exists(oldConfig) {
		if exists(ConfigFile) {
			return nil, fmt.Errorf("both %s and %s exist", oldConfig, ConfigFile)
		}
		old, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(oldConfig)))
		if err != nil {
			return nil, err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(old, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", oldConfig, err)
		}
		if len(doc.Content) == 1 && doc.Content[0].Kind == yaml.MappingNode {
			for _, l := range lifted {
				if err := lift(doc.Content[0], l.path, l.limit, m); err != nil {
					return nil, fmt.Errorf("%s: %w", oldConfig, err)
				}
			}
		}
		body, err := yamlfmt.Encode(&doc, old)
		if err != nil {
			return nil, err
		}
		m.Write[ConfigFile] = body
		m.Remove = append(m.Remove, oldConfig)
	}
	if exists("plugins") {
		if exists(PluginsDir) {
			return nil, fmt.Errorf("both plugins/ and %s exist", PluginsDir)
		}
		m.Rename["plugins"] = PluginsDir
	}
	if exists(oldCron) {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(oldCron)))
		if err != nil {
			return nil, err
		}
		if m.Schedules, err = h.ReadSchedules(body); err != nil {
			return nil, fmt.Errorf("%s: %w", oldCron, err)
		}
		m.Remove = append(m.Remove, oldCron)
	}
	if exists("hermes") {
		m.Remove = append(m.Remove, "hermes") // removed only if nothing else is left in it
	}
	return m, nil
}

// lift takes the setting at path out of cfg into m's limits, with its comments.
func lift(cfg *yaml.Node, path, limit string, m *engine.Migration) error {
	parts := strings.Split(path, ".")
	parent := cfg
	for _, p := range parts[:len(parts)-1] {
		if parent = mapValue(parent, p); parent == nil || parent.Kind != yaml.MappingNode {
			return nil
		}
	}
	key := parts[len(parts)-1]
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		k, v := parent.Content[i], parent.Content[i+1]
		switch limit {
		case "reasoning":
			m.Limits.Reasoning = v.Value
		default:
			n, err := strconv.Atoi(v.Value)
			if err != nil {
				return fmt.Errorf("%s is not a whole number", path)
			}
			switch limit {
			case "turns":
				m.Limits.Turns = n
			case "command_timeout":
				m.Limits.CommandTimeout = n
			case "script_timeout":
				m.Limits.ScriptTimeout = n
			}
		}
		comment := strings.TrimSpace(strings.Join([]string{k.HeadComment, v.LineComment, k.LineComment}, "\n"))
		if comment != "" {
			m.Comments[limit] = comment
		}
		parent.Content = append(parent.Content[:i], parent.Content[i+2:]...)
		return nil
	}
	return nil
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
