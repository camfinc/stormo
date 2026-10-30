package instance

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/capability"
)

// Capability is a heavy or shared call the core performs for agents (docs/remote-core.md §6):
// declared by the instance (stormo.yaml capabilities:), granted per agent (agent.yaml
// capabilities:), brokered by the core (queue, concurrency, metering, audit), and run by a
// worker, never inside the core: an HTTP endpoint, a one-shot container, or a service container the
// core boots on the first job and puts to sleep when idle, on this host or on a runner.
type Capability struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// http | container | service
	Kind string `json:"kind"`
	// Template: a bundled template (pkg/capability) the rest is filled from; its image is built here.
	Template string `json:"template,omitempty"`
	// http: the endpoint the core POSTs {job, agent, input} to; headers may name the core's
	// secrets as ${NAME} (secrets.local.yaml shared layer).
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// container: the image and command, run with the input as JSON on stdin and its JSON answer
	// on stdout; Secrets are env var names it gets from the secrets file of the host running it.
	Image     string   `json:"image,omitempty"`
	Command   []string `json:"command,omitempty"`
	Secrets   []string `json:"secrets,omitempty"`
	Placement string   `json:"placement,omitempty"` // container, service: local (default) or a runner placement
	// service: the HTTP port inside the container, its readiness path, minutes without jobs before it
	// sleeps, minutes it may take to boot, memory and /dev/shm in MB (0: docker's default), CPUs
	// (0: no cap), and plain settings it gets in its environment (secrets go in Secrets).
	Port        int               `json:"port,omitempty"`
	Health      string            `json:"health,omitempty"`
	IdleMinutes int               `json:"idleMinutes,omitempty"`
	BootMinutes int               `json:"bootMinutes,omitempty"`
	MemoryMB    int               `json:"memoryMB,omitempty"`
	ShmMB       int               `json:"shmMB,omitempty"`
	CPUs        float64           `json:"cpus,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	// What agents read to explore it (cap_describe): usage, a JSON schema of the input, examples.
	Docs        string           `json:"docs,omitempty"`
	InputSchema map[string]any   `json:"inputSchema,omitempty"`
	Examples    []map[string]any `json:"examples,omitempty"`
	// Files: none | in | out | both: jobs take files from the agent's shared space, give files back.
	// Only where the core reaches the worker's disk: container and service on this host.
	Files string `json:"files"`
	// Unavailable says why it cannot run here (a template this engine does not ship): kept, not
	// fatal, so an older binary reading a newer stormo.yaml (an agent's sidecar) still loads it;
	// `stormo check` refuses it and the core refuses its jobs.
	Unavailable string `json:"unavailable,omitempty"`
	// Jobs at once (default 1), minutes one may take (default 15), and the estimated cost of one
	// in USD (for metering; 0 is unknown).
	Concurrency    int     `json:"concurrency"`
	TimeoutMinutes int     `json:"timeoutMinutes"`
	CostUSD        float64 `json:"costUSD"`
}

type rawCapability struct {
	Description    string            `yaml:"description"`
	Kind           string            `yaml:"kind"`
	Template       string            `yaml:"template"`
	Port           int               `yaml:"port"`
	Health         string            `yaml:"health"`
	IdleMinutes    int               `yaml:"idle_minutes"`
	BootMinutes    int               `yaml:"boot_minutes"`
	MemoryMB       int               `yaml:"memory_mb"`
	ShmMB          int               `yaml:"shm_mb"`
	CPUs           float64           `yaml:"cpus"`
	Env            map[string]string `yaml:"env"`
	Docs           string            `yaml:"docs"`
	InputSchema    map[string]any    `yaml:"input_schema"`
	Examples       []map[string]any  `yaml:"examples"`
	Files          string            `yaml:"files"`
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
		where := "capabilities." + n
		if !CapabilityName.MatchString(n) {
			return nil, fmt.Errorf("%s: a name is lowercase words joined by dots", where)
		}
		if r.Template != "" {
			t, ok := capability.Lookup(r.Template)
			if !ok {
				out = append(out, Capability{Name: n, Template: r.Template, Kind: "service", Description: r.Description, Files: "none", Placement: "local",
					Concurrency: 1, TimeoutMinutes: 15, Unavailable: fmt.Sprintf("this engine ships no template %q (%s)", r.Template, strings.Join(capability.Names(), ", "))})
				continue
			}
			r = fromTemplate(r, t)
		}
		c := Capability{Name: n, Description: r.Description, Kind: r.Kind, Template: r.Template, URL: r.URL, Headers: r.Headers, Image: r.Image, Command: r.Command,
			Secrets: r.Secrets, Placement: r.Placement, Concurrency: r.Concurrency, TimeoutMinutes: r.TimeoutMinutes, CostUSD: r.CostUSD,
			Port: r.Port, Health: r.Health, IdleMinutes: r.IdleMinutes, BootMinutes: r.BootMinutes, MemoryMB: r.MemoryMB, ShmMB: r.ShmMB, CPUs: r.CPUs,
			Env: r.Env, Docs: r.Docs, InputSchema: r.InputSchema, Examples: r.Examples, Files: r.Files}
		switch c.Kind {
		case "http":
			if !strings.HasPrefix(c.URL, "https://") && !strings.HasPrefix(c.URL, "http://") {
				return nil, fmt.Errorf("%s: an http capability needs url", where)
			}
		case "container", "service":
			if c.Image == "" {
				return nil, fmt.Errorf("%s: a %s capability needs image (or template)", where, c.Kind)
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
			return nil, fmt.Errorf("%s: kind is http, container or service", where)
		}
		if c.Kind == "service" {
			if c.Port == 0 {
				c.Port = 8080
			}
			if c.Health == "" {
				c.Health = "/health"
			}
			if c.IdleMinutes <= 0 {
				c.IdleMinutes = 10
			}
			if c.BootMinutes <= 0 {
				c.BootMinutes = 5
			}
			if c.Port < 1 || c.Port > 65535 || !strings.HasPrefix(c.Health, "/") || c.IdleMinutes > 24*60 || c.BootMinutes > 120 ||
				c.MemoryMB < 0 || c.ShmMB < 0 || c.CPUs < 0 {
				return nil, fmt.Errorf("%s: port 1-65535, health a path, idle_minutes up to 1440, boot_minutes up to 120, memory_mb, shm_mb and cpus not negative", where)
			}
		} else if c.Port != 0 || c.Health != "" || c.IdleMinutes != 0 || c.BootMinutes != 0 || c.MemoryMB != 0 || c.ShmMB != 0 || c.CPUs != 0 || len(c.Env) > 0 {
			return nil, fmt.Errorf("%s: port, health, idle_minutes, boot_minutes, memory_mb, shm_mb, cpus and env are for kind service", where)
		}
		switch c.Files {
		case "":
			c.Files = "none"
		case "none", "in", "out", "both":
		default:
			return nil, fmt.Errorf("%s: files is none, in, out or both", where)
		}
		if c.Files != "none" && (c.Kind == "http" || c.Placement != "local") {
			return nil, fmt.Errorf("%s: files work only for a container or service on this host (placement local): the core hands them over through its disk", where)
		}
		for k := range c.Env {
			if !envName.MatchString(k) {
				return nil, fmt.Errorf("%s: env %q must be an env var NAME", where, k)
			}
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

// fromTemplate fills what the instance left unset from a bundled template: a service whose image is
// the template's (built on this host; a runner placement builds its own the same way).
func fromTemplate(r rawCapability, t capability.Template) rawCapability {
	if r.Kind == "" {
		r.Kind = "service"
	}
	if r.Image == "" {
		r.Image = capability.Image(t.Name)
	}
	if r.Description == "" {
		r.Description = t.Description
	}
	if r.Docs == "" {
		r.Docs = t.Docs
	}
	if r.InputSchema == nil {
		r.InputSchema = t.InputSchema
	}
	if r.Examples == nil {
		r.Examples = t.Examples
	}
	if r.Files == "" {
		r.Files = t.Files
	}
	pick := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	pick(&r.Port, t.Port)
	pick(&r.Concurrency, t.Concurrency)
	pick(&r.TimeoutMinutes, t.TimeoutMinutes)
	pick(&r.IdleMinutes, t.IdleMinutes)
	pick(&r.BootMinutes, t.BootMinutes)
	pick(&r.MemoryMB, t.MemoryMB)
	pick(&r.ShmMB, t.ShmMB)
	if r.Health == "" {
		r.Health = t.Health
	}
	env := map[string]string{}
	for k, v := range t.Env {
		env[k] = v
	}
	for k, v := range r.Env {
		env[k] = v
	}
	if len(env) > 0 {
		r.Env = env
	}
	return r
}

// TemplateImage names the bundled template a capability's image is built from, when it is that template's.
func (c Capability) TemplateImage() (string, bool) {
	if c.Template == "" || c.Image != capability.Image(c.Template) {
		return "", false
	}
	return c.Template, true
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
