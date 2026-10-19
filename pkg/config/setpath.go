package config

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/yamlfmt"
	"go.yaml.in/yaml/v3"
)

// SetPath sets one value in an agent's agent.yaml or one of its engine files (agents/<id>/engine/…),
// addressed by keys, with "[k=v]" picking the item of a sequence whose k is v. Only the addressed
// node changes: comments, key order and the other items stay. agent.yaml is validated and written
// as Write does; an engine file only has to stay a YAML mapping (it is created when missing).
func SetPath(inst *instance.Instance, agent, file string, keys []string, value any) error {
	if !slicesContains(manifest.AgentIDs(inst.Root), agent) {
		return refuse("unsupported", "unknown agent %q", agent)
	}
	clean := path.Clean(filepath.ToSlash(file))
	engineFile := strings.HasPrefix(clean, "engine/") && !strings.Contains(clean, "..")
	if clean != "agent.yaml" && !engineFile {
		return refuse("unsupported", "%s: only agent.yaml or a file under engine/ is set here", file)
	}
	if len(keys) == 0 {
		return refuse("usage", "%s: no key to set", file)
	}
	rel := "agents/" + agent + "/" + clean
	p := filepath.Join(inst.Root, filepath.FromSlash(rel))
	old, err := os.ReadFile(p)
	if err != nil && !(engineFile && os.IsNotExist(err)) {
		return err
	}
	var doc yaml.Node
	if len(old) > 0 {
		if err := yaml.Unmarshal(old, &doc); err != nil {
			return refuse("invalid", "%s does not parse: %v", rel, err)
		}
	}
	if len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return refuse("invalid", "%s is not a YAML mapping; fix it as text first", rel)
	}
	var v yaml.Node
	if err := v.Encode(value); err != nil {
		return err
	}
	if err := setIn(doc.Content[0], keys, &v); err != nil {
		return refuse("invalid", "%s: %v", rel, err)
	}
	var body []byte
	if len(old) > 0 {
		body, err = yamlfmt.Encode(&doc, old)
	} else {
		body, err = yaml.Marshal(&doc)
	}
	if err != nil {
		return err
	}
	if clean == "agent.yaml" {
		_, err := Write(inst, rel, body, Hash(old))
		return err
	}
	if old == nil {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, body, 0o644)
	}
	return replace(p, body)
}

func setIn(n *yaml.Node, keys []string, v *yaml.Node) error {
	k := keys[0]
	if strings.HasPrefix(k, "[") && strings.HasSuffix(k, "]") {
		field, want, ok := strings.Cut(k[1:len(k)-1], "=")
		if !ok || n.Kind != yaml.SequenceNode {
			return fmt.Errorf("%s: not a list item selector here", k)
		}
		for _, item := range n.Content {
			if item.Kind != yaml.MappingNode {
				continue
			}
			if at := keyIndex(item, field); at >= 0 && item.Content[at+1].Value == want {
				if len(keys) == 1 {
					return fmt.Errorf("%s: a list item is not replaced whole", k)
				}
				return setIn(item, keys[1:], v)
			}
		}
		return fmt.Errorf("no item with %s: %s", field, want)
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: parent is not a mapping", k)
	}
	at := keyIndex(n, k)
	if len(keys) == 1 {
		if at >= 0 {
			n.Content[at+1] = replaceNode(n.Content[at+1], v)
		} else {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, plain(v))
		}
		return nil
	}
	if at < 0 {
		if strings.HasPrefix(keys[1], "[") {
			return fmt.Errorf("%s: no such list", k)
		}
		n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"})
		at = len(n.Content) - 2
	}
	return setIn(n.Content[at+1], keys[1:], v)
}

func slicesContains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
