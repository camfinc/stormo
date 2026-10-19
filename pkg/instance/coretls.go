package instance

import (
	"fmt"
	"net"
	"strings"

	"go.yaml.in/yaml/v3"
)

// CoreTLS is stormo.yaml core.tls: `off`, or the certificate and key files (relative to the
// instance) the core serves its non-loopback listener with.
type CoreTLS struct {
	Off  bool   `json:"off,omitempty"`
	Cert string `json:"cert,omitempty"`
	Key  string `json:"key,omitempty"`
}

// parseCoreTLS reads core.tls and checks it against core.listen: a listener beyond loopback has
// TLS, or TLS off on a private address only (a tunnel or VPN carries the encryption).
func parseCoreTLS(n *yaml.Node, listen string) (CoreTLS, error) {
	var t CoreTLS
	switch {
	case n.Kind == 0:
	case n.Kind == yaml.ScalarNode && (n.Value == "off" || n.Value == "false"):
		t.Off = true
	case n.Kind == yaml.MappingNode:
		var m struct {
			Cert string `yaml:"cert"`
			Key  string `yaml:"key"`
		}
		if err := n.Decode(&m); err != nil {
			return t, fmt.Errorf("core.tls: %v", err)
		}
		if m.Cert == "" || m.Key == "" {
			return t, fmt.Errorf("core.tls needs both cert and key (files), or `off`")
		}
		t.Cert, t.Key = m.Cert, m.Key
	default:
		return t, fmt.Errorf("core.tls is `off` or {cert, key}")
	}
	if listen == "" {
		return t, nil
	}
	ip := net.ParseIP(strings.Trim(listen, "[]"))
	if ip == nil {
		return t, fmt.Errorf("core.listen must be an IP address (the interface to listen on), not %q", listen)
	}
	if ip.IsUnspecified() && t.Off {
		return t, fmt.Errorf("core.listen %s with core.tls off would serve every interface in clear text; name the private address", listen)
	}
	if !t.Off && t.Cert == "" {
		return t, fmt.Errorf("core.listen needs core.tls: {cert, key}, or core.tls: off on a private address")
	}
	if t.Off && !PrivateAddress(ip) {
		return t, fmt.Errorf("core.tls off is only allowed on a private address (a tunnel or VPN encrypts); %s is public", listen)
	}
	return t, nil
}

var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// PrivateAddress: loopback, RFC 1918, IPv6 ULA, link-local, or CGNAT (Tailscale's 100.64/10).
func PrivateAddress(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

func trimURLs(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := map[string]string{}
	for k, v := range m {
		out[k] = strings.TrimRight(v, "/")
	}
	return out
}

// CoreURLFor is the core's base URL for agents in a placement (local, aws, vps…): core.urls[p],
// else core.url, else "" (a local core: the caller uses host.docker.internal and the port).
func (i *Instance) CoreURLFor(placement string) string {
	if u := i.CoreURLs[placement]; u != "" {
		return u
	}
	return i.CoreURL
}
