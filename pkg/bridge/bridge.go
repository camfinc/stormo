// Package bridge is how agents act on the organisation's systems without holding broad
// credentials in their own sandbox. Each action is typed, owned by a unit (or the group), and
// exposed to an agent only if the agent's manifest lists it. Transport (MCP server in the task /
// HTTP behind the ALB / SQS for triggers) is an open decision in ARCHITECTURE.md; the contract is
// transport-independent and mirrors MCP tool definitions so it can be served as one directly.
//
// An instance declares its actions in YAML (stormo.yaml `bridge.actions: bridge/actions.yaml`):
//
//   - name: sales.crm.deals.list          # <unit|group>.<system>.<resource>.<verb>
//     description: List deals (read-only).
//     inputSchema: {type: object, properties: {dateFrom: {type: string}}}
//     secrets: [CRM_API_TOKEN]            # held by the bridge, not the agent
//     mutates: false                      # true needs an explicit approval path
//     http:
//     method: GET
//     url: https://crm.example.com/api/deals
//     auth: {bearer_secret: CRM_API_TOKEN}
//     query: from_input                 # from_input (default) | none
package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"go.yaml.in/yaml/v3"
)

// HTTP is a declarative handler: one HTTP call per invocation.
type HTTP struct {
	Method string `yaml:"method" json:"method"`
	URL    string `yaml:"url" json:"url"`
	Auth   struct {
		BearerSecret string `yaml:"bearer_secret" json:"bearer_secret,omitempty"`
	} `yaml:"auth" json:"auth"`
	// from_input: non-empty input fields become the query string (schema property order). none:
	// no query; non-GET methods send the input as a JSON body.
	Query string `yaml:"query" json:"query"`
}

// Action is one bridge action.
type Action struct {
	Name        string         `yaml:"name" json:"name"`
	Description string         `yaml:"description" json:"description"`
	InputSchema map[string]any `yaml:"inputSchema" json:"inputSchema"`
	Secrets     []string       `yaml:"secrets" json:"secrets"`
	Mutates     bool           `yaml:"mutates" json:"mutates"`
	HTTP        *HTTP          `yaml:"http" json:"http,omitempty"`
	// Property order of inputSchema.properties, for a stable query string.
	order []string
}

// Load reads the instance's actions (stormo.yaml bridge.actions); none when unset.
func Load(inst *instance.Instance) ([]*Action, error) {
	if inst.BridgeActions == "" {
		return []*Action{}, nil
	}
	path := filepath.Join(inst.Root, inst.BridgeActions)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
	default:
		return nil, fmt.Errorf("bridge.actions must name a YAML file, got %s (docs/instances.md)", inst.BridgeActions)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var nodes yaml.Node
	if err := yaml.Unmarshal(body, &nodes); err != nil {
		return nil, fmt.Errorf("%s: %w", inst.BridgeActions, err)
	}
	var actions []*Action
	if err := yaml.Unmarshal(body, &actions); err != nil {
		return nil, fmt.Errorf("%s: must be a list of actions: %w", inst.BridgeActions, err)
	}
	seen := map[string]bool{}
	for i, a := range actions {
		where := fmt.Sprintf("%s: action %d", inst.BridgeActions, i+1)
		if a.Name == "" || len(strings.Split(a.Name, ".")) < 2 {
			return nil, fmt.Errorf("%s: name must be <unit|group>.<system>.<resource>.<verb>", where)
		}
		if seen[a.Name] {
			return nil, fmt.Errorf("%s: %s is declared twice", where, a.Name)
		}
		seen[a.Name] = true
		if a.Secrets == nil {
			a.Secrets = []string{}
		}
		if a.InputSchema == nil {
			a.InputSchema = map[string]any{"type": "object"}
		}
		if h := a.HTTP; h != nil {
			if h.Method == "" {
				h.Method = http.MethodGet
			}
			h.Method = strings.ToUpper(h.Method)
			if h.Query == "" {
				h.Query = "from_input"
			}
			if h.Query != "from_input" && h.Query != "none" {
				return nil, fmt.Errorf("%s (%s): http.query must be from_input or none", where, a.Name)
			}
			if h.URL == "" {
				return nil, fmt.Errorf("%s (%s): http.url is required", where, a.Name)
			}
			if s := h.Auth.BearerSecret; s != "" && !contains(a.Secrets, s) {
				return nil, fmt.Errorf("%s (%s): http.auth.bearer_secret %s must be listed under secrets:", where, a.Name, s)
			}
		}
		if len(nodes.Content) == 1 && i < len(nodes.Content[0].Content) {
			a.order = propertyOrder(nodes.Content[0].Content[i])
		}
	}
	return actions, nil
}

func propertyOrder(action *yaml.Node) []string {
	schema := child(action, "inputSchema")
	props := child(schema, "properties")
	out := []string{}
	for i := 0; props != nil && i+1 < len(props.Content); i += 2 {
		out = append(out, props.Content[i].Value)
	}
	return out
}

func child(m *yaml.Node, key string) *yaml.Node {
	for i := 0; m != nil && m.Kind == yaml.MappingNode && i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Unit is the owning unit (or "group") of an action name.
func Unit(name string) string { return strings.SplitN(name, ".", 2)[0] }

// ActionsFor: actions an agent may call, listed in its manifest and owned by its unit or the group.
func ActionsFor(a *manifest.Agent, registry []*Action) ([]*Action, error) {
	known := map[string]*Action{}
	for _, x := range registry {
		known[x.Name] = x
	}
	out := []*Action{}
	for _, name := range a.Actions {
		x, ok := known[name]
		if !ok {
			return nil, fmt.Errorf("%s: unknown action %s", a.ID, name)
		}
		if u := Unit(name); u != a.Unit && u != "group" {
			return nil, fmt.Errorf("%s: %s belongs to unit %s", a.ID, name, u)
		}
		out = append(out, x)
	}
	return out, nil
}

// Context is what an invocation runs with: the bridge's own secrets and an HTTP client.
type Context struct {
	Env  map[string]string
	Doer interface {
		Do(*http.Request) (*http.Response, error)
	}
}

// Invoke runs an action for an agent, enforcing the allow-list again at call time.
func Invoke(a *manifest.Agent, registry []*Action, name string, input map[string]any, ctx Context) (any, error) {
	allowed, err := ActionsFor(a, registry)
	if err != nil {
		return nil, err
	}
	var action *Action
	for _, x := range allowed {
		if x.Name == name {
			action = x
		}
	}
	if action == nil {
		return nil, fmt.Errorf("%s may not call %s", a.ID, name)
	}
	h := action.HTTP
	if h == nil {
		return nil, fmt.Errorf("%s has no handler", name)
	}
	u, err := url.Parse(h.URL)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if h.Query == "from_input" {
		parts := []string{}
		for _, k := range inputOrder(action, input) {
			v := input[k]
			if v == nil || v == "" || v == false {
				continue
			}
			parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(fmt.Sprint(v)))
		}
		if len(parts) > 0 {
			u.RawQuery = strings.Join(parts, "&")
		}
	} else if h.Method != http.MethodGet {
		b, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(h.Method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s := h.Auth.BearerSecret; s != "" {
		token := ctx.Env[s]
		if token == "" {
			return nil, fmt.Errorf("%s is not configured on the bridge", s)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	doer := ctx.Doer
	if doer == nil {
		doer = http.DefaultClient
	}
	res, err := doer.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("%s %d", name, res.StatusCode)
	}
	var out any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%s: response is not JSON: %w", name, err)
	}
	return out, nil
}

// inputOrder is the schema's property order, then any other input keys sorted.
func inputOrder(a *Action, input map[string]any) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, k := range a.order {
		if _, ok := input[k]; ok {
			out = append(out, k)
			seen[k] = true
		}
	}
	rest := []string{}
	for k := range input {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// MCPTool is the MCP `tools/list` shape.
type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// MCPTools lists an agent's allowed actions as MCP tools.
func MCPTools(a *manifest.Agent, registry []*Action) ([]MCPTool, error) {
	allowed, err := ActionsFor(a, registry)
	if err != nil {
		return nil, err
	}
	out := []MCPTool{}
	for _, x := range allowed {
		out = append(out, MCPTool{strings.ReplaceAll(x.Name, ".", "_"), x.Description, x.InputSchema})
	}
	return out, nil
}
