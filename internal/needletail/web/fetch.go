// Package web is the agent's reach into the public web (docs/needletail.md §4.5): web_extract
// fetches a public http(s) page and gives its readable text, web_search asks a search provider
// (when the agent has one). What either returns is untrusted web content: the run that reads it is
// marked (§7, information flow), since fetched pages are the classic prompt-injection vector.
package web

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/netguard"
)

// Bounds of a fetch.
const (
	// MaxFetch is the most of a page's (decompressed) body read; the rest is not.
	MaxFetch = 2 << 20
	// MaxRedirects is how many redirects are followed, each checked again.
	MaxRedirects = 3
	// FetchTimeout bounds one fetch, redirects included.
	FetchTimeout = 20 * time.Second
	userAgent    = "needletail-web/1 (+an AI agent reading a page it was asked to)"
)

// Fetcher gets public pages. It resolves a host once, refuses any internal or metadata address
// among the answers (netguard) and dials the address it checked, follows redirects itself (each
// checked again, at most MaxRedirects), never through a proxy, sends no cookie and no credential,
// and never reaches the swarm core. A zero Fetcher is ready.
type Fetcher struct {
	// CoreURLs are the core's and its relay's addresses (the MCP and ingest URLs, SWARM_CORE_URL):
	// never fetched, even if public.
	CoreURLs []string

	// test hooks: how names resolve, and where a checked address is dialled
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	tls     *tls.Config
}

// Response is a fetched page.
type Response struct {
	// URL is where the page was found (after redirects).
	URL         string
	Status      int
	ContentType string
	Body        []byte
	// Truncated: the page was longer than MaxFetch.
	Truncated bool
}

// Error is a refused or failed fetch; its message never carries a body.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errorf(format string, args ...any) error { return &Error{fmt.Sprintf(format, args...)} }

// textual is a content type a page's text can be read from.
func textual(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		mt = strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	}
	switch {
	case mt == "", strings.HasPrefix(mt, "text/"), mt == "application/xhtml+xml", mt == "application/json", mt == "application/xml",
		strings.HasSuffix(mt, "+json"), strings.HasSuffix(mt, "+xml"):
		return true
	}
	return false
}

// checkURL refuses anything but an absolute http(s) URL with no credentials and a valid port, to a
// host that is not the core's. The URL it returns has its host in the form the checks compare.
func (f *Fetcher) checkURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, errorf("%q is not an absolute http(s) URL", clip(raw, 200))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errorf("only http and https pages are fetched (%s)", u.Scheme)
	}
	if u.User != nil {
		return nil, errorf("credentials in the URL are not allowed")
	}
	host, port, ok := netguard.HostPort(u)
	if !ok {
		return nil, errorf("the URL has no valid host and port (1-65535)")
	}
	if u.Port() != "" {
		u.Host = net.JoinHostPort(host, strconv.Itoa(port))
	} else if strings.Contains(host, ":") {
		u.Host = "[" + host + "]"
	} else {
		u.Host = host
	}
	if f.isCore(host, port) {
		return nil, errorf("the swarm core is not fetched")
	}
	u.Fragment = ""
	return u, nil
}

// isCore: host:port (normalized) is one of the core's (or its relay's) addresses.
func (f *Fetcher) isCore(host string, port int) bool {
	for _, s := range f.CoreURLs {
		cu, err := url.Parse(s)
		if err != nil || cu.Host == "" {
			continue
		}
		if ch, cp, ok := netguard.HostPort(cu); ok && ch == host && cp == port {
			return true
		}
	}
	return false
}

// coreAddr: ip:port is where the core listens.
func (f *Fetcher) coreAddr(ctx context.Context, ip netip.Addr, port int) bool {
	for _, s := range f.CoreURLs {
		cu, err := url.Parse(s)
		if err != nil || cu.Host == "" {
			continue
		}
		ch, cp, ok := netguard.HostPort(cu)
		if !ok || cp != port {
			continue
		}
		addrs, _ := f.lookup(ctx, ch)
		for _, a := range addrs {
			if a.Unmap() == ip.Unmap() {
				return true
			}
		}
	}
	return false
}

func (f *Fetcher) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}, nil
	}
	if f.resolve != nil {
		return f.resolve(ctx, host)
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// client dials only an address it resolved and checked: none internal, none metadata, not the
// core; no proxy, no cookie jar, no redirect followed by itself.
func (f *Fetcher) client() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: FetchTimeout,
		DisableKeepAlives:     true,
		TLSClientConfig:       f.tls,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, portText, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, ok := netguard.ParsePort(portText)
			if !ok {
				return nil, errorf("%q is not a port", portText)
			}
			addrs, err := f.lookup(ctx, host)
			if err != nil {
				return nil, errorf("%s could not be resolved", host)
			}
			if len(addrs) == 0 {
				return nil, errorf("%s has no address", host)
			}
			for _, a := range addrs {
				switch {
				case netguard.Metadata(a):
					return nil, errorf("%s resolves to a metadata address: refused", host)
				case netguard.Internal(a):
					return nil, errorf("%s resolves to an internal address: refused (only public pages are fetched)", host)
				case f.coreAddr(ctx, a, port):
					return nil, errorf("%s is the swarm core: refused", host)
				}
			}
			target := net.JoinHostPort(addrs[0].Unmap().String(), strconv.Itoa(port))
			if f.dial != nil {
				return f.dial(ctx, network, target)
			}
			return dialer.DialContext(ctx, network, target)
		},
	}
	return &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Get fetches a public page: at most MaxFetch bytes of its body, a textual content type only.
func (f *Fetcher) Get(ctx context.Context, raw string) (*Response, error) {
	u, err := f.checkURL(raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, FetchTimeout)
	defer cancel()
	client := f.client()
	for hop := 0; ; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, errorf("the request could not be made")
		}
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
		res, err := client.Do(req)
		if err != nil {
			var fe *Error
			switch {
			case errors.As(err, &fe):
				return nil, fe
			case ctx.Err() != nil:
				return nil, errorf("%s timed out", u.Host)
			}
			return nil, errorf("%s could not be reached", u.Host)
		}
		if loc := res.Header.Get("Location"); isRedirect(res.StatusCode) && loc != "" {
			res.Body.Close()
			if hop+1 > MaxRedirects {
				return nil, errorf("more than %d redirects: refused", MaxRedirects)
			}
			next, err := u.Parse(loc)
			if err != nil {
				return nil, errorf("a bad redirect")
			}
			if u, err = f.checkURL(next.String()); err != nil {
				return nil, err
			}
			continue
		}
		defer res.Body.Close()
		ct := res.Header.Get("Content-Type")
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return nil, errorf("%s answered HTTP %d", u.Host, res.StatusCode)
		}
		if !textual(ct) {
			mt, _, _ := mime.ParseMediaType(ct)
			return nil, errorf("not a text page (%s): only HTML and text are read", clip(mt, 80))
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, MaxFetch+1))
		if err != nil && len(body) == 0 {
			return nil, errorf("%s: the page could not be read", u.Host)
		}
		out := &Response{URL: u.String(), Status: res.StatusCode, ContentType: ct, Body: body}
		if len(body) > MaxFetch {
			out.Body, out.Truncated = body[:MaxFetch], true
		}
		return out, nil
	}
}

func isRedirect(code int) bool {
	return code == 301 || code == 302 || code == 303 || code == 307 || code == 308
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}
