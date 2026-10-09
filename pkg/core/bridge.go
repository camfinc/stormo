package core

import (
	"net"
	"os"
	"runtime"
	"strings"
)

// Agent containers reach the core as host.docker.internal. OrbStack and Docker Desktop forward
// that name to the host's loopback, so the loopback listener is enough there. Docker Engine on
// Linux maps it (extra_hosts host-gateway) to the docker0 bridge address, which a loopback-only
// listener never sees: there the core also listens on the bridge, serving only what agents call.

// BridgeHosts are the extra addresses the core listens on for containers: SWARM_CORE_BIND
// (comma-separated; "none" turns it off), else docker0's IPv4 on Linux, else none.
func BridgeHosts() []string {
	return bridgeHosts(os.Getenv("SWARM_CORE_BIND"), runtime.GOOS, docker0)
}

func bridgeHosts(env, goos string, iface func() string) []string {
	env = strings.TrimSpace(env)
	switch {
	case env == "none":
		return nil
	case env != "":
		hosts := []string{}
		for _, h := range strings.Split(env, ",") {
			h = strings.TrimSpace(h)
			// The loopback listener is always there; listening twice would fail.
			if ip := net.ParseIP(h); h == "" || h == "localhost" || (ip != nil && ip.IsLoopback()) {
				continue
			}
			hosts = append(hosts, h)
		}
		return hosts
	case goos == "linux":
		if ip := iface(); ip != "" {
			return []string{ip}
		}
	}
	return nil
}

// docker0 is the default bridge's IPv4, what host-gateway resolves to; "" without one.
func docker0() string {
	ifc, err := net.InterfaceByName("docker0")
	if err != nil {
		return ""
	}
	addrs, _ := ifc.Addrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return ""
}

// bridgeRoute is what a bridge listener serves: what agents call, each keyed or signed per agent
// (the model gateway, MCP, hook ingest) and /health. The UI, /api/* and avatars stay on loopback.
// A new route agents call must be added here too, or it works on macOS and 404s on Linux.
func bridgeRoute(path string) bool {
	return path == "/health" || path == "/mcp" || strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/ingest/")
}
