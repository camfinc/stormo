// Package netguard is the runtime's address checks for outbound HTTP the agent asks for (Luau's
// nt.http, web_extract): which addresses are internal (loopback, RFC 1918, CGNAT, link-local, ULA,
// multicast, reserved and documentation ranges), the cloud metadata addresses, and hosts and ports
// in the one normalized form every check compares (docs/needletail.md §5.3, §7).
package netguard

import (
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// metadata addresses are never dialled, whatever is granted.
var metadata = []netip.Addr{netip.MustParseAddr("169.254.169.254"), netip.MustParseAddr("fd00:ec2::254")}

var internalNets = func() []netip.Prefix {
	out := []netip.Prefix{}
	for _, s := range []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/128", "::1/128", "64:ff9b::/96", "100::/64", "2001:db8::/32", "fc00::/7", "fe80::/10", "ff00::/8"} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

// Metadata: a is a cloud metadata address (IPv4-mapped forms too).
func Metadata(a netip.Addr) bool {
	a = a.Unmap()
	for _, m := range metadata {
		if m == a {
			return true
		}
	}
	return false
}

// Internal: an address the agent's outbound HTTP never reaches unless something grants it by its
// exact IP (loopback, RFC 1918, CGNAT, link-local, ULA, multicast, reserved and documentation
// ranges, and their IPv4-mapped forms).
func Internal(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range internalNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// NormHost is a host as every check compares it: lower case, no trailing dot, an IP literal in its
// canonical form.
func NormHost(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.String()
	}
	return h
}

// ParsePort is a port as a number: decimal digits only, 1 to 65535 (0443 is 443).
func ParsePort(s string) (int, bool) {
	if s == "" || len(s) > 8 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 65535
}

// HostPort is u's normalized host and its port as a number (the scheme's default when it has none).
func HostPort(u *url.URL) (string, int, bool) {
	host := NormHost(u.Hostname())
	if host == "" {
		return "", 0, false
	}
	if p := u.Port(); p != "" {
		n, ok := ParsePort(p)
		return host, n, ok
	}
	switch u.Scheme {
	case "https", "wss":
		return host, 443, true
	case "http", "ws":
		return host, 80, true
	}
	return "", 0, false
}
