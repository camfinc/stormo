package luau

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Cap is one capability (wire.Capability): Kind fs.read, fs.write, http, core, message, memory,
// kv or followup, and its Arg (home, shared, home/<glob>, a host or *.suffix, a tool or prefix*, a
// send_message target or <prefix>:*, read or write).
type Cap struct{ Kind, Arg string }

func (c Cap) String() string { return c.Kind + ":" + c.Arg }

// ParseCap reads one capability.
func ParseCap(s string) (Cap, error) {
	s = strings.TrimSpace(s)
	if !wire.Capability.MatchString(s) {
		return Cap{}, fmt.Errorf("%q is not a capability (fs.read:home, fs.write:home[/glob], fs.read:shared, fs.write:shared, http:<host>, core:<tool>, message:<target>, memory:read, memory:write, kv:read, kv:write, followup:read, followup:write, route:<agent>|route:slack:<target>)", s)
	}
	kind, arg, _ := strings.Cut(s, ":")
	return Cap{Kind: kind, Arg: arg}, nil
}

func parseCaps(list []string) ([]Cap, error) {
	out := []Cap{}
	for _, s := range list {
		c, err := ParseCap(s)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Caps are a run's effective capabilities: a call is allowed when both what the tool declares and
// what the agent is granted (needletail.yaml, outside anything the agent can write) allow it.
type Caps struct{ Declared, Granted []Cap }

// NewCaps parses the declared and granted lists.
func NewCaps(declared, granted []string) (Caps, error) {
	d, err := parseCaps(declared)
	if err != nil {
		return Caps{}, err
	}
	g, err := parseCaps(granted)
	if err != nil {
		return Caps{}, fmt.Errorf("grant: %w", err)
	}
	return Caps{Declared: d, Granted: g}, nil
}

func allowsAny(list []Cap, kind string, match func(arg string) bool) bool {
	for _, c := range list {
		if c.Kind == kind && match(c.Arg) {
			return true
		}
	}
	return false
}

func (c Caps) allow(kind string, match func(arg string) bool) bool {
	return allowsAny(c.Declared, kind, match) && allowsAny(c.Granted, kind, match)
}

// FSRead: reading scope (home or shared).
func (c Caps) FSRead(scope string) bool {
	return c.allow("fs.read", func(a string) bool { return a == scope })
}

// FSWrite: writing rel (slash-separated, relative to the home) or the shared space (scope shared).
func (c Caps) FSWrite(scope, rel string) bool {
	return c.allow("fs.write", func(a string) bool { return writeCovers(a, scope, rel) })
}

func writeCovers(arg, scope, rel string) bool {
	if scope == "shared" {
		return arg == "shared"
	}
	if arg == "home" {
		return true
	}
	glob, ok := strings.CutPrefix(arg, "home/")
	if !ok {
		return false
	}
	m, err := doublestar.Match(glob, rel)
	return err == nil && m
}

// HTTP: a request to host.
func (c Caps) HTTP(host string) bool {
	return c.allow("http", func(a string) bool { return hostMatch(a, host) })
}

// HTTPExact: host named exactly (no wildcard) by both lists; what lets a request reach an internal
// address that is otherwise refused.
func (c Caps) HTTPExact(host string) bool {
	host = normHost(host)
	return c.allow("http", func(a string) bool { return normHost(a) == host })
}

func hostMatch(pattern, host string) bool {
	host = normHost(host)
	if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
		return strings.HasSuffix(host, "."+suffix)
	}
	return pattern == host
}

// Message: sending to target (messaging.Target.String's form: slack, slack:<channel>, …).
func (c Caps) Message(target string) bool {
	return c.allow("message", func(a string) bool { return messaging.GrantMatches(a, target) })
}

// Memory: the memory archive's op, read (search_memory) or write (save, update, delete_memory).
// Neither implies the other, nor anything of fs.*.
func (c Caps) Memory(op string) bool {
	return c.allow("memory", func(a string) bool { return a == op })
}

// KV: the key-value store's op, read (kv_get) or write (kv_set, kv_delete). Neither implies the
// other.
func (c Caps) KV(op string) bool {
	return c.allow("kv", func(a string) bool { return a == op })
}

// FollowUp: the one-time follow-ups' op, read (nt.followup.list) or write (follow_up,
// nt.followup.cancel). Neither implies the other; a follow-up to another target than the run's
// conversation needs that target's message: cap besides.
func (c Caps) FollowUp(op string) bool {
	return c.allow("followup", func(a string) bool { return a == op })
}

// Route: a handler's route action to target (an agent id, or a Slack target in Target.String's
// form): route:<agent>, route:slack:<target>, a <prefix>:* form, or route:*.
func (c Caps) Route(target string) bool {
	return c.allow("route", func(a string) bool { return messaging.GrantMatches(a, target) })
}

// Core: calling the core's tool.
func (c Caps) Core(tool string) bool {
	return c.allow("core", func(a string) bool { return toolMatch(a, tool) })
}

func toolMatch(pattern, tool string) bool {
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(tool, prefix)
	}
	return pattern == tool
}

// Effective is what the declared caps come to under the grants, for display: each declared cap a
// grant covers, else the grants narrower than it.
func (c Caps) Effective() []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(x Cap) {
		if !seen[x.String()] {
			seen[x.String()] = true
			out = append(out, x.String())
		}
	}
	for _, d := range c.Declared {
		covered := false
		for _, g := range c.Granted {
			if covers(g, d) {
				add(d)
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		for _, g := range c.Granted {
			if covers(d, g) {
				add(g)
			}
		}
	}
	return out
}

// covers: a allows every call b allows.
func covers(a, b Cap) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case "fs.write":
		if a.Arg == b.Arg || a.Arg == "home" && strings.HasPrefix(b.Arg, "home") {
			return true
		}
		ga, okA := strings.CutPrefix(a.Arg, "home/")
		gb, okB := strings.CutPrefix(b.Arg, "home/")
		if okA && okB {
			m, err := doublestar.Match(ga, gb)
			return err == nil && m
		}
		return false
	case "http":
		if a.Arg == b.Arg {
			return true
		}
		if strings.HasPrefix(a.Arg, "*.") {
			return hostMatch(a.Arg, strings.TrimPrefix(b.Arg, "*."))
		}
		return false
	case "message":
		return messaging.GrantMatches(a.Arg, b.Arg)
	case "core":
		if a.Arg == b.Arg {
			return true
		}
		if prefix, ok := strings.CutSuffix(a.Arg, "*"); ok {
			return strings.HasPrefix(strings.TrimSuffix(b.Arg, "*"), prefix)
		}
		return false
	}
	return a.Arg == b.Arg
}
