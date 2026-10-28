package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// fake is a Fetcher whose names resolve as the test says and whose dials, once every check passed,
// reach srv (a loopback test server standing in for the public host).
func fake(t *testing.T, srv *httptest.Server, names map[string][]string) *Fetcher {
	t.Helper()
	f := &Fetcher{}
	f.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, s := range names[host] {
			out = append(out, netip.MustParseAddr(s))
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no such host %s", host)
		}
		return out, nil
	}
	f.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	return f
}

const public = "93.184.216.34"

func TestFetchPage(t *testing.T) {
	var cookies atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
			cookies.Add(1)
		}
		switch r.URL.Path {
		case "/start":
			http.SetCookie(w, &http.Cookie{Name: "s", Value: "1"})
			http.Redirect(w, r, "http://www.example.test/page", http.StatusFound)
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, "<title>Hi</title><p>Hello web</p>")
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, strings.Repeat("a", MaxFetch+100))
		case "/pdf":
			w.Header().Set("Content-Type", "application/pdf")
			fmt.Fprint(w, "%PDF")
		case "/missing":
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	f := fake(t, srv, map[string][]string{"example.test": {public}, "www.example.test": {public}})
	r, err := f.Get(context.Background(), "http://example.test/start")
	if err != nil {
		t.Fatal(err)
	}
	if r.URL != "http://www.example.test/page" || !strings.Contains(string(r.Body), "Hello web") || r.Status != 200 {
		t.Errorf("fetched %+v", r)
	}
	if cookies.Load() != 0 {
		t.Error("a cookie or credential was sent")
	}
	if r, err = f.Get(context.Background(), "http://example.test/big"); err != nil || len(r.Body) != MaxFetch || !r.Truncated {
		t.Errorf("big: %v %d %v", err, len(r.Body), r != nil && r.Truncated)
	}
	if _, err = f.Get(context.Background(), "http://example.test/pdf"); err == nil || !strings.Contains(err.Error(), "application/pdf") {
		t.Errorf("pdf: %v", err)
	}
	if _, err = f.Get(context.Background(), "http://example.test/missing"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404: %v", err)
	}
}

// SSRF: nothing internal, no metadata, no core, no credentials, only http(s), redirects re-checked
// and at most 3, DNS pinned (an answer mixing in an internal address is refused).
func TestFetchRefuses(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/to-internal":
			http.Redirect(w, r, "http://10.1.2.3/x", http.StatusFound)
		case "/to-meta":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/to-ftp":
			http.Redirect(w, r, "ftp://example.test/x", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		}
	}))
	defer srv.Close()
	f := fake(t, srv, map[string][]string{"example.test": {public}, "mixed.test": {public, "127.0.0.1"}, "private.test": {"192.168.1.4"},
		"core.example.test": {public}, "v6.test": {"::ffff:10.0.0.1"}})
	f.CoreURLs = []string{"https://core.example.test:18600/mcp"}
	cases := map[string]string{
		"http://127.0.0.1/":                 "internal",
		"http://[::1]/":                     "internal",
		"http://localhost/":                 "",
		"http://169.254.169.254/":           "metadata",
		"http://private.test/":              "internal",
		"http://mixed.test/":                "internal",
		"http://v6.test/":                   "internal",
		"https://core.example.test:18600/x": "core",
		"ftp://example.test/":               "http",
		"http://user:pw@example.test/":      "credentials",
		"http://example.test/to-internal":   "internal",
		"http://example.test/to-meta":       "metadata",
		"http://example.test/to-ftp":        "http",
		"http://example.test/loop":          "redirects",
		"http://example.test:0/":            "port",
		"file:///etc/passwd":                "http",
		"example.test/no-scheme":            "",
	}
	for u, want := range cases {
		_, err := f.Get(context.Background(), u)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", u, err, want)
		}
	}
	// the refused redirects never reached a second server; the loop stopped after 3 more hops
	if n := hits.Load(); n > 4+4 {
		t.Errorf("%d requests reached the server", n)
	}
}
