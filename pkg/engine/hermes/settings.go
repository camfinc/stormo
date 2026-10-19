package hermes

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/manifest"
	"go.yaml.in/yaml/v3"
)

// The settings a Hermes agent changes at runtime, all in config.yaml:
//   - home_channel.<platform>: /sethome writes platforms.<platform>.home_channel. Stormo hands the
//     manifest's home channel over as <PLATFORM>_HOME_CHANNEL, which Hermes reads before
//     config.yaml, so a runtime one lasts only until the next boot unless it reaches agent.yaml.
//   - command_allowlist: commands approved "always"; sensitive.
//   - mcp_servers.<name>: MCP servers it configured; sensitive. Their env and headers are left out:
//     Hermes keeps credentials in .env, which is never napped, and they belong in secrets.

const (
	homeKey      = "home_channel."
	allowlistKey = "command_allowlist"
	mcpKey       = "mcp_servers."
)

// Settings reads the runtime-changeable settings from a config.yaml.
func (h *Hermes) Settings(config []byte) ([]engine.Setting, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(config, &doc); err != nil {
		return nil, err
	}
	out := []engine.Setting{}
	if platforms, ok := doc["platforms"].(map[string]any); ok {
		for _, kind := range sortedAnyKeys(platforms) {
			p, _ := platforms[kind].(map[string]any)
			home, _ := p["home_channel"].(map[string]any)
			if id := scalarText(home["chat_id"]); id != "" {
				out = append(out, engine.Setting{Key: homeKey + kind, Value: id, Opaque: true})
			}
		}
	}
	out = append(out, sensitiveSettings(doc)...)
	return out, nil
}

func sensitiveSettings(doc map[string]any) []engine.Setting {
	out := []engine.Setting{}
	if list, ok := doc[allowlistKey].([]any); ok && len(list) > 0 {
		cmds := make([]string, 0, len(list))
		for _, c := range list {
			cmds = append(cmds, scalarText(c))
		}
		sort.Strings(cmds)
		b, _ := json.Marshal(cmds)
		out = append(out, engine.Setting{Key: allowlistKey, Value: string(b), Sensitive: true})
	}
	if servers, ok := doc["mcp_servers"].(map[string]any); ok {
		for _, name := range sortedAnyKeys(servers) {
			def, ok := servers[name].(map[string]any)
			if !ok {
				continue
			}
			kept := map[string]any{}
			for k, v := range def {
				if k != "env" && k != "headers" {
					kept[k] = v
				}
			}
			b, err := json.Marshal(kept) // map keys sorted: canonical
			if err != nil {
				continue
			}
			out = append(out, engine.Setting{Key: mcpKey + name, Value: string(b), Sensitive: true})
		}
	}
	return out
}

// RepoSettings: home channels from the manifest's channels, the rest from the engine config file.
func (h *Hermes) RepoSettings(a *manifest.Agent, agentDir string) ([]engine.Setting, error) {
	out := []engine.Setting{}
	for _, c := range a.Channels {
		if c.HomeChannel != "" {
			out = append(out, engine.Setting{Key: homeKey + c.Kind, Value: c.HomeChannel, Opaque: true})
		}
	}
	rel, err := engineFile(agentDir, ConfigFile, "hermes/config.base.yaml")
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(filepath.Join(agentDir, filepath.FromSlash(rel)))
	if os.IsNotExist(err) {
		return out, nil
	} else if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	return append(out, sensitiveSettings(doc)...), nil
}

// SettingEdit: a home channel goes to the channel's entry in agent.yaml; the rest to the engine
// config file the agent's config.yaml is generated from.
func (h *Hermes) SettingEdit(a *manifest.Agent, s engine.Setting) (*engine.SettingEdit, error) {
	switch {
	case strings.HasPrefix(s.Key, homeKey):
		kind := strings.TrimPrefix(s.Key, homeKey)
		for _, c := range a.Channels {
			if c.Kind == kind {
				return &engine.SettingEdit{File: "agent.yaml", Path: []string{"channels", "[kind=" + kind + "]", "home_channel"}, Value: s.Value}, nil
			}
		}
		return nil, fmt.Errorf("%s: the agent has no %s channel in agent.yaml", s.Key, kind)
	case s.Key == allowlistKey:
		var cmds []string
		if err := json.Unmarshal([]byte(s.Value), &cmds); err != nil {
			return nil, fmt.Errorf("%s: %w", s.Key, err)
		}
		return &engine.SettingEdit{File: ConfigFile, Path: []string{allowlistKey}, Value: cmds}, nil
	case strings.HasPrefix(s.Key, mcpKey):
		var def map[string]any
		if err := json.Unmarshal([]byte(s.Value), &def); err != nil {
			return nil, fmt.Errorf("%s: %w", s.Key, err)
		}
		return &engine.SettingEdit{File: ConfigFile, Path: []string{"mcp_servers", strings.TrimPrefix(s.Key, mcpKey)}, Value: def}, nil
	}
	return nil, fmt.Errorf("%s: not a setting Hermes agents change", s.Key)
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func scalarText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}
