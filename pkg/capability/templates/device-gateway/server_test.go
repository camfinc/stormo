package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCore plays the core's /api/ingress/* and records every call it gets.
type fakeCore struct {
	mu      sync.Mutex
	state   string // the pairing's state
	pub     jwk
	revoked bool
	calls   []string
	bodies  []string
	headers []http.Header
}

func (f *fakeCore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
	f.bodies = append(f.bodies, string(b))
	f.headers = append(f.headers, r.Header.Clone())
	if r.Header.Get("Authorization") != "Bearer sti_000000000000_"+strings.Repeat("a", 48) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"code":"unauthorized"}}`))
		return
	}
	device := func() bool {
		if f.revoked || r.Header.Get("X-Stormo-Device") != "dev_0123456789abcdef" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"code":"device_unknown","message":"this device is not linked"}}`))
			return false
		}
		return true
	}
	switch {
	case r.URL.Path == "/api/ingress/pairings":
		var req map[string]any
		json.Unmarshal(b, &req)
		k, _ := json.Marshal(req["publicKey"])
		json.Unmarshal(k, &f.pub)
		w.WriteHeader(201)
		w.Write([]byte(`{"pairing":"pr_0123456789abcdef","userCode":"WDJB-MJHT","fingerprint":"B756-48F2-D3AD","expiresIn":600}`))
	case r.URL.Path == "/api/ingress/pairings/pr_0123456789abcdef":
		out := map[string]string{"state": f.state}
		if f.state == "approved" {
			out["deviceId"], out["name"] = "dev_0123456789abcdef", "Watch"
			f.state = "consumed"
		}
		json.NewEncoder(w).Encode(out)
	case r.URL.Path == "/api/ingress/devices/dev_0123456789abcdef":
		if f.revoked {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":{"code":"device_unknown"}}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": "dev_0123456789abcdef", "publicKey": f.pub})
	case r.URL.Path == "/api/ingress/me":
		if device() {
			w.Write([]byte(`{"device":{"id":"dev_0123456789abcdef","name":"Watch"},"instance":"Acme"}`))
		}
	case r.URL.Path == "/api/ingress/fleet":
		if device() {
			w.Write([]byte(`{"agents":[{"id":"support","name":"Ada"}],"polledAt":null}`))
		}
	case strings.HasPrefix(r.URL.Path, "/api/ingress/"):
		if device() {
			w.Write([]byte(`{"ok":true}`))
		}
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeCore) last() (string, string, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1], f.bodies[len(f.bodies)-1], f.headers[len(f.headers)-1]
}

type rig struct {
	t    *testing.T
	core *fakeCore
	gw   *gateway
	srv  *httptest.Server
	now  time.Time
}

func newRig(t *testing.T) *rig {
	core := &fakeCore{state: "pending"}
	cs := httptest.NewServer(core)
	t.Cleanup(cs.Close)
	g := newGateway(config{CoreURL: cs.URL, Key: "sti_000000000000_" + strings.Repeat("a", 48), IPHeader: "Cf-Connecting-Ip", CountryHeader: "Cf-Ipcountry",
		TokenTTL: time.Hour, MaxPending: 20, PerIP: 3})
	rg := &rig{t: t, core: core, gw: g, now: time.Unix(1_700_000_000, 0)}
	g.now = func() time.Time { return rg.now }
	rg.srv = httptest.NewServer(g)
	t.Cleanup(rg.srv.Close)
	return rg
}

func (rg *rig) do(method, path, token string, body any, hdr ...string) (int, map[string]any) {
	rg.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = strings.NewReader(string(j))
	}
	req, _ := http.NewRequest(method, rg.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Cf-Connecting-Ip", "203.0.113.7")
	req.Header.Set("Cf-Ipcountry", "it")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		rg.t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Cache-Control") != "no-store" {
		rg.t.Errorf("%s %s: no Cache-Control: no-store", method, path)
	}
	var out map[string]any
	json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func code(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func keyJWK(k *ecdsa.PrivateKey) map[string]string {
	b, _ := k.PublicKey.Bytes() // uncompressed: 0x04 || x || y
	return map[string]string{"kty": "EC", "crv": "P-256", "x": base64.RawURLEncoding.EncodeToString(b[1:33]), "y": base64.RawURLEncoding.EncodeToString(b[33:])}
}

func sign(t *testing.T, k *ecdsa.PrivateKey, device, nonce string) string {
	d := sha256.Sum256(sessionMessage(device, nonce))
	sig, err := ecdsa.SignASN1(rand.Reader, k, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(sig)
}

func pairBody(k *ecdsa.PrivateKey) map[string]any {
	return map[string]any{"name": "Watch", "model": "Pixel Watch 3", "os": "Wear OS 6", "appVersion": "0.1.0", "publicKey": keyJWK(k)}
}

// pairAndSession links a device and opens a session, as a watch would.
func (rg *rig) pairAndSession(k *ecdsa.PrivateKey) string {
	t := rg.t
	st, p := rg.do("POST", "/device/v1/pair", "", pairBody(k))
	if st != 200 || p["userCode"] != "WDJB-MJHT" || p["interval"] != 5.0 || p["fingerprint"] != "B756-48F2-D3AD" {
		t.Fatalf("pair: %d %v", st, p)
	}
	dc := p["deviceCode"].(string)
	if st, out := rg.do("POST", "/device/v1/pair/poll", "", map[string]string{"deviceCode": dc}); st != 400 || code(out) != "authorization_pending" {
		t.Fatalf("poll pending: %d %v", st, out)
	}
	rg.now = rg.now.Add(5 * time.Second)
	rg.core.mu.Lock()
	rg.core.state = "approved"
	rg.core.mu.Unlock()
	st, out := rg.do("POST", "/device/v1/pair/poll", "", map[string]string{"deviceCode": dc})
	if st != 200 || out["deviceId"] != "dev_0123456789abcdef" {
		t.Fatalf("poll approved: %d %v", st, out)
	}
	rg.now = rg.now.Add(5 * time.Second)
	if st, out := rg.do("POST", "/device/v1/pair/poll", "", map[string]string{"deviceCode": dc}); st != 400 || code(out) != "expired_token" {
		t.Fatalf("a used device code: %d %v", st, out)
	}
	st, ch := rg.do("POST", "/device/v1/session/challenge", "", map[string]string{"deviceId": "dev_0123456789abcdef"})
	if st != 200 || ch["expiresIn"] != 60.0 {
		t.Fatalf("challenge: %d %v", st, ch)
	}
	nonce := ch["nonce"].(string)
	st, s := rg.do("POST", "/device/v1/session", "", map[string]string{"deviceId": "dev_0123456789abcdef", "nonce": nonce, "signature": sign(t, k, "dev_0123456789abcdef", nonce)})
	if st != 200 || !strings.HasPrefix(s["token"].(string), "std_") || s["expiresIn"] != 3600.0 {
		t.Fatalf("session: %d %v", st, s)
	}
	return s["token"].(string)
}

func TestPairSessionAndRoutes(t *testing.T) {
	rg := newRig(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tok := rg.pairAndSession(k)

	// What the gateway told the core about the pairing: the checked key, the edge's IP and country.
	rg.core.mu.Lock()
	var pr map[string]any
	json.Unmarshal([]byte(rg.core.bodies[0]), &pr)
	rg.core.mu.Unlock()
	if pr["ip"] != "203.0.113.7" || pr["country"] != "IT" || pr["name"] != "Watch" {
		t.Errorf("pairing sent to the core: %v", pr)
	}

	st, me := rg.do("GET", "/device/v1/me", tok, nil)
	if st != 200 || me["instance"] != "Acme" || me["tokenExpires"] != float64(rg.now.Add(time.Hour).UnixMilli()) {
		t.Fatalf("me: %d %v", st, me)
	}
	call, _, hdr := rg.core.last()
	if call != "GET /api/ingress/me" || hdr.Get("X-Stormo-Device") != "dev_0123456789abcdef" || hdr.Get("X-Stormo-Client-Ip") != "203.0.113.7" ||
		hdr.Get("Authorization") == "Bearer "+tok {
		t.Errorf("the core's call: %s %v", call, hdr)
	}
	if st, f := rg.do("GET", "/device/v1/fleet", tok, nil); st != 200 || len(f["agents"].([]any)) != 1 {
		t.Errorf("fleet: %d %v", st, f)
	}
	// Typed calls only: a query or extra fields never reach the core.
	rg.do("GET", "/device/v1/agents/support/conversations/abc?limit=7&x=../../tokens", tok, nil)
	if call, _, _ := rg.core.last(); call != "GET /api/ingress/agents/support/conversations/abc?limit=7" {
		t.Errorf("conversation call: %s", call)
	}
	if st, out := rg.do("POST", "/device/v1/agents/support/chat", tok, `{"message":"hi","admin":true}`); st != 400 || code(out) != "bad_request" {
		t.Errorf("unknown field: %d %v", st, out)
	}
	if st, _ := rg.do("POST", "/device/v1/agents/support/chat", tok, map[string]string{"message": "hi"}); st != 200 {
		t.Errorf("chat: %d", st)
	}
	if _, body, _ := rg.core.last(); body != `{"message":"hi"}` {
		t.Errorf("chat body re-encoded: %s", body)
	}
	if st, out := rg.do("POST", "/device/v1/review", tok, map[string]string{"agent": "support", "kind": "lesson", "id": "L-1", "decision": "promote"}); st != 400 || code(out) != "bad_request" {
		t.Errorf("review promote: %d %v", st, out)
	}
	// Nothing outside the device API, and no token: unauthorized.
	for _, p := range []string{"/api/fleet", "/device/v1/../api/tokens", "/health", "/device/v1/agents/../../api/devices/chat"} {
		if st, _ := rg.do("GET", p, tok, nil); st != 404 {
			t.Errorf("%s: %d", p, st)
		}
	}
	if st, out := rg.do("GET", "/device/v1/me", "", nil); st != 401 || code(out) != "unauthorized" {
		t.Errorf("no token: %d %v", st, out)
	}
	if st, out := rg.do("GET", "/device/v1/me", "std_forged", nil); st != 401 || code(out) != "unauthorized" {
		t.Errorf("forged token: %d %v", st, out)
	}
	// Expired.
	rg.now = rg.now.Add(time.Hour + time.Second)
	if st, out := rg.do("GET", "/device/v1/me", tok, nil); st != 401 || code(out) != "unauthorized" {
		t.Errorf("expired token: %d %v", st, out)
	}
	if st, out := rg.do("GET", "/device/v1/health", "", nil); st != 200 || out["status"] != "ok" || len(out) != 1 {
		t.Errorf("health: %d %v", st, out)
	}
}

func TestSessionsNeedTheKeyAndAFreshNonce(t *testing.T) {
	rg := newRig(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rg.pairAndSession(k)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	dev := "dev_0123456789abcdef"
	_, ch := rg.do("POST", "/device/v1/session/challenge", "", map[string]string{"deviceId": dev})
	n := ch["nonce"].(string)
	if st, out := rg.do("POST", "/device/v1/session", "", map[string]string{"deviceId": dev, "nonce": n, "signature": sign(t, other, dev, n)}); st != 401 || code(out) != "bad_signature" {
		t.Errorf("another key: %d %v", st, out)
	}
	// The nonce was used by the failed attempt.
	if st, out := rg.do("POST", "/device/v1/session", "", map[string]string{"deviceId": dev, "nonce": n, "signature": sign(t, k, dev, n)}); st != 400 || code(out) != "expired_nonce" {
		t.Errorf("reused nonce: %d %v", st, out)
	}
	// A new challenge replaces the last; an old one is dead; a stale one expires.
	_, c1 := rg.do("POST", "/device/v1/session/challenge", "", map[string]string{"deviceId": dev})
	_, c2 := rg.do("POST", "/device/v1/session/challenge", "", map[string]string{"deviceId": dev})
	n1, n2 := c1["nonce"].(string), c2["nonce"].(string)
	if st, _ := rg.do("POST", "/device/v1/session", "", map[string]string{"deviceId": dev, "nonce": n1, "signature": sign(t, k, dev, n1)}); st != 400 {
		t.Errorf("replaced nonce: %d", st)
	}
	_, c3 := rg.do("POST", "/device/v1/session/challenge", "", map[string]string{"deviceId": dev})
	n3 := c3["nonce"].(string)
	rg.now = rg.now.Add(61 * time.Second)
	if st, _ := rg.do("POST", "/device/v1/session", "", map[string]string{"deviceId": dev, "nonce": n3, "signature": sign(t, k, dev, n3)}); st != 400 {
		t.Errorf("stale nonce: %d", st)
	}
	_ = n2
	// The signature covers the device id: a nonce signed for another id does not verify.
	_, c4 := rg.do("POST", "/device/v1/session/challenge", "", map[string]string{"deviceId": dev})
	n4 := c4["nonce"].(string)
	if st, out := rg.do("POST", "/device/v1/session", "", map[string]string{"deviceId": dev, "nonce": n4, "signature": sign(t, k, "dev_ffffffffffffffff", n4)}); st != 401 || code(out) != "bad_signature" {
		t.Errorf("signed for another id: %d %v", st, out)
	}
}

func TestARevokedDeviceLosesItsSessions(t *testing.T) {
	rg := newRig(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tok := rg.pairAndSession(k)
	rg.core.mu.Lock()
	rg.core.revoked = true
	rg.core.mu.Unlock()
	if st, out := rg.do("GET", "/device/v1/fleet", tok, nil); st != 401 || code(out) != "device_unknown" {
		t.Fatalf("revoked: %d %v", st, out)
	}
	if st, out := rg.do("GET", "/device/v1/fleet", tok, nil); st != 401 || code(out) != "unauthorized" {
		t.Errorf("the session is gone: %d %v", st, out)
	}
	if st, out := rg.do("POST", "/device/v1/session/challenge", "", map[string]string{"deviceId": "dev_0123456789abcdef"}); st != 401 || code(out) != "device_unknown" {
		t.Errorf("a revoked device gets no challenge: %d %v", st, out)
	}
	rg.gw.mu.Lock()
	n := len(rg.gw.nonces)
	rg.gw.mu.Unlock()
	if n != 0 {
		t.Errorf("challenge state kept for a device that is not linked: %d", n)
	}
}

func TestBodiesAreOneSmallJSONObject(t *testing.T) {
	rg := newRig(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tok := rg.pairAndSession(k)
	for _, body := range []string{
		`{"message":"hi"} {"message":"again"}`,
		`{"message":"hi"}]`,
		`{"message":"hi"}` + strings.Repeat(" ", maxBody),
		`[{"message":"hi"}]`,
	} {
		if st, out := rg.do("POST", "/device/v1/agents/support/chat", tok, body); st != 400 || code(out) != "bad_request" {
			t.Errorf("%.40q: %d %v", body, st, out)
		}
	}
	if st, _ := rg.do("POST", "/device/v1/agents/support/chat", tok, `{"message":"hi"}`+"\n"); st != 200 {
		t.Errorf("a trailing newline is fine: %d", st)
	}
	// A review decision names the version it was shown.
	if st, _ := rg.do("POST", "/device/v1/review", tok, map[string]string{"agent": "support", "kind": "lesson", "id": "L-1", "decision": "accept"}); st != 400 {
		t.Errorf("no version: %d", st)
	}
	if st, _ := rg.do("POST", "/device/v1/review", tok, map[string]string{"agent": "support", "kind": "lesson", "id": "L-1", "version": "0123456789abcdef", "decision": "accept"}); st != 200 {
		t.Errorf("with version: %d", st)
	}
}

func TestPairingPollAndCaps(t *testing.T) {
	rg := newRig(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, p := rg.do("POST", "/device/v1/pair", "", pairBody(k))
	dc := p["deviceCode"].(string)
	rg.do("POST", "/device/v1/pair/poll", "", map[string]string{"deviceCode": dc})
	if st, out := rg.do("POST", "/device/v1/pair/poll", "", map[string]string{"deviceCode": dc}); st != 400 || code(out) != "slow_down" {
		t.Errorf("too fast: %d %v", st, out)
	}
	rg.now = rg.now.Add(10 * time.Second)
	rg.core.mu.Lock()
	rg.core.state = "denied"
	rg.core.mu.Unlock()
	if st, out := rg.do("POST", "/device/v1/pair/poll", "", map[string]string{"deviceCode": dc}); st != 400 || code(out) != "access_denied" {
		t.Errorf("denied: %d %v", st, out)
	}
	if st, out := rg.do("POST", "/device/v1/pair/poll", "", map[string]string{"deviceCode": "nope"}); st != 400 || code(out) != "expired_token" {
		t.Errorf("unknown code: %d %v", st, out)
	}
	// A bad key never reaches the core.
	before := len(rg.core.calls)
	bad := pairBody(k)
	bad["publicKey"] = map[string]string{"kty": "EC", "crv": "P-256", "x": "AQID", "y": "BAUG"}
	if st, _ := rg.do("POST", "/device/v1/pair", "", bad); st != 400 || len(rg.core.calls) != before {
		t.Errorf("bad key: %d, %d calls", st, len(rg.core.calls)-before)
	}
	// Per address, the oldest pending pairing is evicted past the cap.
	codes := []string{}
	for i := 0; i < 4; i++ {
		rg.now = rg.now.Add(time.Second)
		_, p := rg.do("POST", "/device/v1/pair", "", pairBody(k))
		codes = append(codes, p["deviceCode"].(string))
	}
	rg.gw.mu.Lock()
	n := len(rg.gw.pairings)
	_, first := rg.gw.pairings[hash(codes[0])]
	rg.gw.mu.Unlock()
	if n != 3 || first {
		t.Errorf("per-address cap: %d pending, oldest kept %v", n, first)
	}
}

func TestRateLimits(t *testing.T) {
	rg := newRig(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	for i := 0; i < 10; i++ {
		if st, _ := rg.do("POST", "/device/v1/pair", "", pairBody(k)); st != 200 {
			t.Fatalf("pair %d: %d", i, st)
		}
	}
	st, out := rg.do("POST", "/device/v1/pair", "", pairBody(k))
	if st != 429 || code(out) != "rate_limited" {
		t.Fatalf("eleventh pairing: %d %v", st, out)
	}
	// Another address has its own budget.
	if st, _ := rg.do("POST", "/device/v1/pair", "", pairBody(k), "Cf-Connecting-Ip", "198.51.100.1"); st != 200 {
		t.Errorf("another address: %d", st)
	}
	rg.now = rg.now.Add(time.Minute)
	if st, _ := rg.do("POST", "/device/v1/pair", "", pairBody(k)); st != 200 {
		t.Errorf("a minute later: %d", st)
	}
}

func TestConfigNeedsTheCoresKey(t *testing.T) {
	t.Setenv("STORMO_CORE_URL", "")
	t.Setenv("STORMO_INGRESS_KEY", "")
	if _, err := configFromEnv(); err == nil {
		t.Error("no key: refused")
	}
	t.Setenv("STORMO_CORE_URL", "http://host.docker.internal:18601/")
	t.Setenv("STORMO_INGRESS_KEY", "sti_x")
	t.Setenv("TOKEN_MINUTES", "2")
	if _, err := configFromEnv(); err == nil {
		t.Error("TOKEN_MINUTES under 5: refused")
	}
	t.Setenv("TOKEN_MINUTES", "30")
	c, err := configFromEnv()
	if err != nil || c.CoreURL != "http://host.docker.internal:18601" || c.TokenTTL != 30*time.Minute {
		t.Errorf("config: %+v %v", c, err)
	}
}

// A passing core error during a challenge leaves the device's sessions alone; only an unlinked device loses them.
func TestOnlyAConfirmedUnlinkDropsSessions(t *testing.T) {
	rg := newRig(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tok := rg.pairAndSession(k)
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/ingress/devices/") {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":{"code":"core_internal"}}`))
			return
		}
		rg.core.ServeHTTP(w, r)
	}))
	defer broken.Close()
	rg.gw.cfg.CoreURL = broken.URL
	if st, _ := rg.do("POST", "/device/v1/session/challenge", "", map[string]string{"deviceId": "dev_0123456789abcdef"}); st != 500 {
		t.Errorf("challenge during a core error: %d", st)
	}
	if st, _ := rg.do("GET", "/device/v1/me", tok, nil); st != 200 {
		t.Errorf("the session survived a passing error: %d", st)
	}
}

// The core's request for a pause reaches the device with its Retry-After.
func TestCoreRetryAfterIsPassedOn(t *testing.T) {
	rg := newRig(t)
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tok := rg.pairAndSession(k)
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/ingress/review" {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"code":"rate_limited"}}`))
			return
		}
		rg.core.ServeHTTP(w, r)
	}))
	defer busy.Close()
	rg.gw.cfg.CoreURL = busy.URL
	req, _ := http.NewRequest("GET", rg.srv.URL+"/device/v1/review", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 429 || res.Header.Get("Retry-After") != "7" {
		t.Errorf("busy core: %d Retry-After %q", res.StatusCode, res.Header.Get("Retry-After"))
	}
}
