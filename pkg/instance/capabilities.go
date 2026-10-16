package instance

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Capability is a heavy or shared call the core performs for agents (docs/remote-core.md §6):
// declared by the instance (stormo.yaml capabilities:), granted per agent (agent.yaml
// capabilities:), brokered by the core (queue, concurrency, metering, audit), and run by a
// worker, never inside the core: an HTTP endpoint, or a container on this host or on a runner.
type Capability struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// http | container
	Kind string `json:"kind"`
	// http: the endpoint the core POSTs {job, agent, input} to; headers may name the core's
	// secrets as ${NAME} (secrets.local.yaml shared layer).
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// container: the image and command, run with the input as JSON on stdin and its JSON answer
	// on stdout; Secrets are env var names it gets from the secrets file of the host running it.
	Image     string   `json:"image,omitempty"`
	Command   []string `json:"command,omitempty"`
	Secrets   []string `json:"secrets,omitempty"`
	Placement string   `json:"placement,omitempty"` // container: local (default) or a runner placement
	// Jobs at once (default 1), minutes one may take (default 15), and the estimated cost of one
	// in USD (for metering; 0 is unknown).
	Concurrency    int     `json:"concurrency"`
	TimeoutMinutes int     `json:"timeoutMinutes"`
	CostUSD        float64 `json:"costUSD"`
}

type rawCapability struct {
	Description    string            `yaml:"description"`
	Kind           string            `yaml:"kind"`
	URL            string            `yaml:"url"`
	Headers        map[string]string `yaml:"headers"`
	Image          string            `yaml:"image"`
	Command        []string          `yaml:"command"`
	Secrets        []string          `yaml:"secrets"`
	Placement      string            `yaml:"placement"`
	Concurrency    int               `yaml:"concurrency"`
	TimeoutMinutes int               `yaml:"timeout_minutes"`
	CostUSD        float64           `yaml:"cost_usd"`
}

// CapabilityName: lowercase words joined by dots or hyphens (media.bake, web-scrape).
var CapabilityName = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z][a-z0-9-]*)*$`)

var envName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

func parseCapabilities(raw map[string]rawCapability, placements []Placement) ([]Capability, error) {
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	sort.Strings(names)
	out := []Capability{}
	for _, n := range names {
		r := raw[n]
		c := Capability{Name: n, Description: r.Description, Kind: r.Kind, URL: r.URL, Headers: r.Headers, Image: r.Image, Command: r.Command,
			Secrets: r.Secrets, Placement: r.Placement, Concurrency: r.Concurrency, TimeoutMinutes: r.TimeoutMinutes, CostUSD: r.CostUSD}
		where := "capabilities." + n
		if !CapabilityName.MatchString(n) {
			return nil, fmt.Errorf("%s: a name is lowercase words joined by dots", where)
		}
		switch c.Kind {
		case "http":
			if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
				return nil, fmt.Errorf("%s: an http capability needs url", where)
			}
		case "container":
			if c.Image == "" {
				return nil, fmt.Errorf("%s: a container capability needs image", where)
			}
			if c.Placement == "" {
				c.Placement = "local"
			}
			ok := false
			for _, p := range placements {
				ok = ok || p.Name == c.Placement && (p.Kind == PlacementLocal || p.Kind == PlacementRunner)
			}
			if !ok {
				return nil, fmt.Errorf("%s: placement %q is not local or a runner placement", where, c.Placement)
			}
		default:
			return nil, fmt.Errorf("%s: kind is http or container", where)
		}
		for _, s := range c.Secrets {
			if !envName.MatchString(s) {
				return nil, fmt.Errorf("%s: secret %q must be an env var NAME", where, s)
			}
		}
		if c.Concurrency <= 0 {
			c.Concurrency = 1
		}
		if c.TimeoutMinutes <= 0 {
			c.TimeoutMinutes = 15
		}
		if c.Concurrency > 64 || c.TimeoutMinutes > 24*60 || c.CostUSD < 0 {
			return nil, fmt.Errorf("%s: concurrency up to 64, timeout_minutes up to 1440, cost_usd not negative", where)
		}
		out = append(out, c)
	}
	return out, nil
}

// CapabilityNamed finds a declared capability.
func (i *Instance) CapabilityNamed(name string) (Capability, bool) {
	for _, c := range i.Capabilities {
		if c.Name == name {
			return c, true
		}
	}
	return Capability{}, false
}
