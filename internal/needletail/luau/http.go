package luau

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// nt.http bounds.
const (
	maxRedirects = 3
	maxBody      = 5 << 20
	callCap      = 20 * time.Second
	maxReqBody   = 4 << 20
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

// internal: an address no tool reaches unless it is granted by its exact IP (loopback, RFC 1918,
// CGNAT, link-local, ULA, multicast, reserved and documentation ranges).
func internal(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range internalNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

type httpArgs struct {
	Method    string            `json:"method"`
	URL       string            `json:"url"`
	Headers   map[string]string `json:"headers"`
	Body      *string           `json:"body"`
	TimeoutMS int               `json:"timeout_ms"`
}

// sensitive headers are dropped when a redirect leaves the host they were set for.
var sensitive = []string{"authorization", "cookie", "proxy-authorization"}

func httpRequest(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	var a httpArgs
	if err := json.Unmarshal(in, &a); err != nil {
		return nil, errf("bad_args", "http.request: %v (url and method strings, headers a table of strings, body a string)", err)
	}
	if a.Method == "" {
		a.Method = http.MethodGet
	}
	a.Method = strings.ToUpper(a.Method)
	switch a.Method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		return nil, errf("bad_args", "http.request: method %q", a.Method)
	}
	if a.Body != nil && len(*a.Body) > maxReqBody {
		return nil, errf("too_large", "http.request: the body is over %d bytes", maxReqBody)
	}
	timeout := callCap
	if a.TimeoutMS > 0 && time.Duration(a.TimeoutMS)*time.Millisecond < timeout {
		timeout = time.Duration(a.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx = context.WithValue(ctx, callKey{}, c) // the dialer checks addresses against c's caps

	u, err := c.checkURL(a.URL)
	if err != nil {
		return nil, err
	}
	origin := normHost(u.Hostname())
	client := c.rt.httpClient()
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, a.Method, u.String(), nil)
		if err != nil {
			return nil, errf("bad_args", "http.request: %v", err)
		}
		if a.Body != nil && (a.Method != "GET" && a.Method != "HEAD") {
			req.Body = io.NopCloser(strings.NewReader(*a.Body))
			req.ContentLength = int64(len(*a.Body))
		}
		for k, v := range a.Headers {
			lk := strings.ToLower(k)
			if lk == "host" || lk == "content-length" || lk == "transfer-encoding" || lk == "connection" {
				continue
			}
			if normHost(u.Hostname()) != origin && contains(sensitive, lk) {
				continue
			}
			req.Header.Set(k, v)
		}
		c.rt.inject(req)
		res, err := client.Do(req)
		if err != nil {
			var he *hostError
			if errors.As(err, &he) {
				return nil, he
			}
			if ctx.Err() != nil {
				return nil, errf("timeout", "http.request: %s timed out", u.Host)
			}
			return nil, errf("http", "http.request %s: %v", u.Host, unwrapURL(err))
		}
		if loc := res.Header.Get("Location"); isRedirect(res.StatusCode) && loc != "" {
			res.Body.Close()
			if hop+1 > maxRedirects {
				return nil, errf("http", "http.request: more than %d redirects", maxRedirects)
			}
			next, err := u.Parse(loc)
			if err != nil {
				return nil, errf("http", "http.request: bad redirect: %v", err)
			}
			if u, err = c.checkURL(next.String()); err != nil {
				return nil, err
			}
			if res.StatusCode == http.StatusSeeOther || (res.StatusCode == 301 || res.StatusCode == 302) && a.Method == "POST" {
				a.Method, a.Body = "GET", nil
			}
			continue
		}
		defer res.Body.Close()
		body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
		if err != nil {
			return nil, errf("http", "http.request %s: reading the body: %v", u.Host, err)
		}
		if len(body) > maxBody {
			return nil, errf("too_large", "http.request: the response is over %d bytes", maxBody)
		}
		headers := map[string]string{}
		for k, v := range res.Header {
			headers[strings.ToLower(k)] = strings.Join(v, ", ")
		}
		return map[string]any{"status": res.StatusCode, "headers": headers, "body": string(body)}, nil
	}
}

func isRedirect(code int) bool {
	return code == 301 || code == 302 || code == 303 || code == 307 || code == 308
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// normHost is a host as every check compares it: lower case, no trailing dot, an IP literal in its
// canonical form.
func normHost(h string) string {
	h = strings.ToLower(strings.TrimSuffix(h, "."))
	if ip, err := netip.ParseAddr(h); err == nil {
		return ip.String()
	}
	return h
}

// parsePort is a port as a number: decimal digits only, 1 to 65535 (0443 is 443).
func parsePort(s string) (int, bool) {
	if s == "" || len(s) > 8 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 1 && n <= 65535
}

// hostPort is u's normalized host and its port as a number (the scheme's default when it has none).
func hostPort(u *url.URL) (string, int, bool) {
	host := normHost(u.Hostname())
	if host == "" {
		return "", 0, false
	}
	if p := u.Port(); p != "" {
		n, ok := parsePort(p)
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

// checkURL refuses anything but HTTPS to a host the tool may reach. The URL it returns has its
// host and port in the form every later check compares (the dialer sees the port as a number).
func (c *Call) checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, errf("bad_args", "http.request: %q is not an absolute URL", raw)
	}
	if u.Scheme != "https" {
		return nil, errf("denied", "http.request: only https is allowed (%s)", u.Scheme)
	}
	if u.User != nil {
		return nil, errf("bad_args", "http.request: credentials in the URL are not allowed")
	}
	host, port, ok := hostPort(u)
	if !ok {
		return nil, errf("bad_args", "http.request: %q has no valid host and port (1-65535)", raw)
	}
	if u.Port() != "" {
		u.Host = net.JoinHostPort(host, strconv.Itoa(port))
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	if !c.Caps.HTTP(host) {
		return nil, c.deny("http.request", "http:"+host)
	}
	if c.rt.isCore(host, port) {
		return nil, errf("denied", "http.request: the swarm core is reached with nt.core, not http")
	}
	return u, nil
}

// isCore: host:port (normalized) is one of the core's (or its relay's) addresses.
func (rt *Runtime) isCore(host string, port int) bool {
	host = normHost(host)
	for _, s := range rt.CoreURLs {
		cu, err := url.Parse(s)
		if err != nil || cu.Host == "" {
			continue
		}
		if ch, cp, ok := hostPort(cu); ok && ch == host && cp == port {
			return true
		}
	}
	return false
}

// coreAddr: ip:port is where the core listens.
func (rt *Runtime) coreAddr(ctx context.Context, ip netip.Addr, port int) bool {
	for _, s := range rt.CoreURLs {
		cu, err := url.Parse(s)
		if err != nil || cu.Host == "" {
			continue
		}
		ch, cp, ok := hostPort(cu)
		if !ok || cp != port {
			continue
		}
		addrs, _ := rt.lookup(ctx, ch)
		for _, a := range addrs {
			if a.Unmap() == ip.Unmap() {
				return true
			}
		}
	}
	return false
}

func (rt *Runtime) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	if rt.resolve != nil {
		return rt.resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// inject adds the http_secrets headers for the request's host; nothing else ever sees them.
func (rt *Runtime) inject(req *http.Request) {
	host := normHost(req.URL.Hostname())
	for _, s := range rt.HTTPSecrets {
		if !hostMatch(normHost(s.Host), host) || rt.Secret == nil {
			continue
		}
		v := rt.Secret(s.Env)
		if v == "" {
			continue
		}
		t := s.Template
		if t == "" {
			t = "{value}"
		}
		req.Header.Set(s.Header, strings.ReplaceAll(t, "{value}", v))
	}
}

type callKey struct{}

// httpClient dials only the address it resolved and checked, never through a proxy, and follows
// no redirect itself (httpRequest does, checking each).
func (rt *Runtime) httpClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: callCap,
		MaxIdleConns:          1,
		DisableKeepAlives:     true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, portText, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, ok := parsePort(portText)
			if !ok {
				return nil, errf("bad_args", "http.request: %q is not a port", portText)
			}
			c, _ := ctx.Value(callKey{}).(*Call)
			addrs, err := rt.lookup(ctx, host)
			if err != nil {
				return nil, err
			}
			if len(addrs) == 0 {
				return nil, fmt.Errorf("%s has no address", host)
			}
			ip := addrs[0]
			for _, a := range addrs {
				if contains2(metadata, a.Unmap()) {
					return nil, errf("denied", "http.request: %s resolves to a metadata address", host)
				}
				if internal(a) && (c == nil || !isIPHost(host) || !c.Caps.HTTPExact(host)) {
					return nil, errf("denied", "http.request: %s resolves to an internal address (%s); grant the exact IP to reach it", host, a)
				}
				if rt.coreAddr(ctx, a, port) {
					return nil, errf("denied", "http.request: %s is the swarm core; use nt.core", host)
				}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
		},
	}
	if rt.rootCAs != nil {
		tr.TLSClientConfig = rt.rootCAs.Clone()
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func isIPHost(h string) bool { _, err := netip.ParseAddr(h); return err == nil }

func contains2(list []netip.Addr, a netip.Addr) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}
