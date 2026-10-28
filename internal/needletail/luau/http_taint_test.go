package luau

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Item 15 (Claude M6): a tainted run's http_request carries none of the agent's http_secrets and
// may only read (GET, HEAD): a write to the outside world (POST, PUT, PATCH, DELETE, OPTIONS)
// is refused (no person approves a script's request; the model may ask in a fresh conversation).
func TestTaintedHTTP(t *testing.T) {
	rt, _ := newRuntime(t)
	var mu sync.Mutex
	var seen []string
	_, base := tlsServer(t, rt, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" auth="+r.Header.Get("Authorization"))
		mu.Unlock()
		fmt.Fprint(w, "ok")
	})
	rt.Grants = append(rt.Grants, "http:127.0.0.1")
	rt.HTTPSecrets = []wire.HTTPSecret{{Host: "127.0.0.1", Env: "API_TOKEN", Header: "Authorization", Template: "Bearer {value}"}}
	rt.Untrusted = func(context.Context, string) string { return "this conversation has taken in words you may not obey" }
	caps := []string{"http:127.0.0.1"}
	res, _ := exec(t, rt, caps, fmt.Sprintf(`return {http_request{url = %q}.status, http_request{method = "HEAD", url = %q}.status}`, base+"/a", base+"/b"))
	wantValue(t, res, `[200, 200]`)
	for _, m := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		fails(t, rt, caps, fmt.Sprintf(`return http_request{method = %q, url = %q, body = "data"}`, m, base+"/w"), "refused")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("requests reached the server: %v", seen)
	}
	for _, s := range seen {
		if s != "GET auth=" && s != "HEAD auth=" {
			t.Errorf("a tainted request carried a secret or wrote: %s", s)
		}
	}
}
