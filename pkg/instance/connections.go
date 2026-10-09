package instance

import (
	"fmt"
	"regexp"
	"strings"
)

// A connection is a named way to reach models (stormo.yaml connections:): an API provider, keyed by
// a secret whose NAME the connection gives (values stay in the secrets files), or a ChatGPT sign-in
// the core holds (local runs only). Agents pick one by name (agent.yaml model.provider, and
// model.local.connection for the core route).

// Connection kinds.
const (
	KindChatGPT    = "chatgpt"
	KindOpenRouter = "openrouter"
	KindOpenAI     = "openai"
	KindAnthropic  = "anthropic"
	KindCustom     = "custom" // any OpenAI-compatible API: base_url and key required
)

// ConnectionKind is what a kind brings by default.
type ConnectionKind struct {
	Kind    string `json:"kind"`
	Label   string `json:"label"`
	BaseURL string `json:"baseUrl,omitempty"`
	Key     string `json:"key,omitempty"`
	// API: reached with a key (any target); else a sign-in the core holds (local runs only).
	API bool `json:"api"`
}

// ConnectionKinds are the kinds a connection can be, in the order a picker shows them.
var ConnectionKinds = []ConnectionKind{
	{KindChatGPT, "ChatGPT plan (signed in on the core)", "", "", false},
	{KindOpenRouter, "OpenRouter", "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY", true},
	{KindOpenAI, "OpenAI API", "https://api.openai.com/v1", "OPENAI_API_KEY", true},
	{KindAnthropic, "Anthropic API", "https://api.anthropic.com/v1/", "ANTHROPIC_API_KEY", true},
	{KindCustom, "Other OpenAI-compatible API", "", "", true},
}

// KindOf is the kind named k.
func KindOf(k string) (ConnectionKind, bool) {
	for _, c := range ConnectionKinds {
		if c.Kind == k {
			return c, true
		}
	}
	return ConnectionKind{}, false
}

// Connection is one stormo.yaml connections: entry with its kind's defaults filled.
type Connection struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"baseUrl,omitempty"`
	// Key is the API key's env var NAME (API kinds).
	Key string `json:"key,omitempty"`
	// Implicit: not written in stormo.yaml (one of the two every instance has).
	Implicit bool `json:"implicit,omitempty"`
}

// API reports whether c is reached with a key (else it is a core sign-in).
func (c Connection) API() bool { k, _ := KindOf(c.Kind); return k.API }

// DefaultChatGPT is the connection the core's original sign-in is (.swarm/core/auth/chatgpt.json).
const DefaultChatGPT = "chatgpt"

// implicit are the connections every instance has unless stormo.yaml defines one by that name:
// what agents used before connections existed (model.provider: openrouter, the core's sign-in).
var implicit = []Connection{
	{Name: KindOpenRouter, Kind: KindOpenRouter, BaseURL: "https://openrouter.ai/api/v1", Key: "OPENROUTER_API_KEY", Implicit: true},
	{Name: DefaultChatGPT, Kind: KindChatGPT, Implicit: true},
}

var (
	connNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	envNameRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
)

// Connection is the instance's connection named name.
func (i *Instance) Connection(name string) (Connection, bool) {
	for _, c := range i.Connections {
		if c.Name == name {
			return c, true
		}
	}
	return Connection{}, false
}

type rawConnection struct {
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"`
	BaseURL string `yaml:"base_url"`
	Key     string `yaml:"key"`
}

func connectionsOf(raw []rawConnection, path string) ([]Connection, error) {
	out := []Connection{}
	seen := map[string]bool{}
	for i, r := range raw {
		at := fmt.Sprintf("%s: connections[%d]", path, i)
		k, ok := KindOf(r.Kind)
		switch {
		case !connNameRe.MatchString(r.Name):
			return nil, &Error{at + ": name is a lowercase id (letters, digits, -)"}
		case seen[r.Name]:
			return nil, &Error{fmt.Sprintf("%s: %s is defined twice", at, r.Name)}
		case !ok:
			kinds := []string{}
			for _, k := range ConnectionKinds {
				kinds = append(kinds, k.Kind)
			}
			return nil, &Error{fmt.Sprintf("%s: kind must be one of %s", at, strings.Join(kinds, ", "))}
		}
		c := Connection{Name: r.Name, Kind: r.Kind, BaseURL: r.BaseURL, Key: r.Key}
		if k.API {
			if c.BaseURL == "" {
				c.BaseURL = k.BaseURL
			}
			if c.Key == "" {
				c.Key = k.Key
			}
			switch {
			case c.BaseURL == "" || !strings.HasPrefix(c.BaseURL, "https://") && !strings.HasPrefix(c.BaseURL, "http://"):
				return nil, &Error{at + ": base_url is required (http:// or https://)"}
			case !envNameRe.MatchString(c.Key):
				return nil, &Error{at + ": key is the API key's env var NAME (e.g. MYAPI_API_KEY); its value goes in the secrets"}
			}
		} else if c.BaseURL != "" || c.Key != "" {
			return nil, &Error{at + ": a chatgpt connection has no base_url or key (it signs in on the core)"}
		}
		seen[r.Name] = true
		out = append(out, c)
	}
	for _, c := range implicit {
		if !seen[c.Name] {
			out = append(out, c)
		}
	}
	return out, nil
}
