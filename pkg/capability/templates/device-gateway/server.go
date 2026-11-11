// The device gateway (docs/devices.md): the one thing an instance exposes so linked devices (a
// watch, a phone) can sign in and reach the fleet while the core stays on loopback. It serves
// /device/v1/* only and holds one credential, the ingress key the core minted for this boot
// (STORMO_INGRESS_KEY), which reaches the core's /api/ingress/* and nothing else. It is not a
// reverse proxy: every call to the core is built here from input checked here. Pairings, nonces,
// sessions and rate limits live in memory; a restart ends every session (devices open new ones).
//
// Standard library only, so it can be read in one sitting.
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxBody        = 16 << 10
	maxPairBody    = 96 << 10
	maxCoreAnswer  = 2 << 20
	nonceTTL       = time.Minute
	pollInterval   = 5 * time.Second
	sessionsPerDev = 5
	maxLimiterKeys = 100_000
	maxNonces      = 10_000
)

type config struct {
	CoreURL, Key            string
	IPHeader, CountryHeader string
	TokenTTL                time.Duration
	MaxPending, PerIP       int
}

func configFromEnv() (config, error) {
	c := config{CoreURL: strings.TrimRight(os.Getenv("STORMO_CORE_URL"), "/"), Key: os.Getenv("STORMO_INGRESS_KEY"),
		IPHeader: os.Getenv("CLIENT_IP_HEADER"), CountryHeader: os.Getenv("CLIENT_COUNTRY_HEADER"), TokenTTL: time.Hour, MaxPending: 20, PerIP: 3}
	if c.CoreURL == "" || !strings.HasPrefix(c.Key, "sti_") {
		return c, errors.New("STORMO_CORE_URL and STORMO_INGRESS_KEY come from the core: run this as a capability (template device-gateway)")
	}
	num := func(name string, def, min, max int) (int, error) {
		v := os.Getenv(name)
		if v == "" {
			return def, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < min || n > max {
			return 0, fmt.Errorf("%s is a number from %d to %d", name, min, max)
		}
		return n, nil
	}
	m, err := num("TOKEN_MINUTES", 60, 5, 24*60)
	if err != nil {
		return c, err
	}
	c.TokenTTL = time.Duration(m) * time.Minute
	if c.MaxPending, err = num("MAX_PENDING", 20, 1, 1000); err != nil {
		return c, err
	}
	if c.PerIP, err = num("MAX_PENDING_PER_IP", 3, 1, 100); err != nil {
		return c, err
	}
	return c, nil
}

type pairing struct {
	core     string // the core's pairing id
	ip       string
	created  time.Time
	expires  time.Time
	interval time.Duration
	lastPoll time.Time
}

type nonce struct {
	value   string
	expires time.Time
}

type session struct {
	device  string
	expires time.Time
}

type window struct {
	start time.Time
	n     int
}

type gateway struct {
	cfg  config
	core *http.Client
	now  func() time.Time

	mu       sync.Mutex
	pairings map[string]*pairing // by sha256(deviceCode)
	nonces   map[string]nonce    // by device
	sessions map[string]session  // by sha256(token)
	windows  map[string]*window
}

func newGateway(cfg config) *gateway {
	return &gateway{cfg: cfg, core: &http.Client{Timeout: 30 * time.Second}, now: time.Now,
		pairings: map[string]*pairing{}, nonces: map[string]nonce{}, sessions: map[string]session{}, windows: map[string]*window{}}
}

func main() {
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	g := newGateway(cfg)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{Addr: ":" + port, Handler: g, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10}
	go g.sweep(time.Minute)
	log.Printf("device gateway on :%s, core at %s", port, cfg.CoreURL)
	log.Fatal(srv.ListenAndServe())
}

// --- helpers ---

type apiError struct {
	status int
	code   string
	msg    string
	retry  int
}

func (e *apiError) Error() string { return e.code + ": " + e.msg }

func fail(status int, code, msg string) *apiError {
	return &apiError{status: status, code: code, msg: msg}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

func writeErr(w http.ResponseWriter, e *apiError) {
	if e.retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.retry))
	}
	writeJSON(w, e.status, map[string]any{"error": map[string]string{"code": e.code, "message": e.msg}})
}

// decode reads a body of at most max bytes holding exactly one JSON value with known fields.
func decode(r *http.Request, max int64, v any) *apiError {
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return fail(400, "bad_request", "could not read the body")
	}
	if int64(len(b)) > max {
		return fail(400, "bad_request", "body too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fail(400, "bad_request", "the body is not the JSON this route takes: "+clip(err.Error(), 120))
	}
	if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
		return fail(400, "bad_request", "one JSON value")
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// clientIP is the device's address: the edge's header when one is configured (the published port is
// loopback-only, so the edge on this host is the only peer), else the peer.
func (g *gateway) clientIP(r *http.Request) string {
	if g.cfg.IPHeader != "" {
		v := strings.TrimSpace(strings.Split(r.Header.Get(g.cfg.IPHeader), ",")[0])
		if ip := net.ParseIP(v); ip != nil {
			return ip.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (g *gateway) country(r *http.Request) string {
	if g.cfg.CountryHeader == "" {
		return ""
	}
	v := strings.ToUpper(strings.TrimSpace(r.Header.Get(g.cfg.CountryHeader)))
	if len(v) == 2 && v[0] >= 'A' && v[0] <= 'Z' && v[1] >= 'A' && v[1] <= 'Z' {
		return v
	}
	return ""
}

// allow counts a request against key's per-minute budget.
func (g *gateway) allow(key string, perMinute int) *apiError {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	w := g.windows[key]
	if w == nil || now.Sub(w.start) >= time.Minute {
		if w == nil && len(g.windows) >= maxLimiterKeys {
			for k, o := range g.windows {
				if now.Sub(o.start) >= time.Minute {
					delete(g.windows, k)
				}
			}
			if len(g.windows) >= maxLimiterKeys {
				// A flood of addresses: refuse newcomers rather than forget the budgets in use.
				e := fail(429, "rate_limited", "too many requests")
				e.retry = 60
				return e
			}
		}
		w = &window{start: now}
		g.windows[key] = w
	}
	w.n++
	if w.n > perMinute {
		e := fail(429, "rate_limited", "too many requests")
		e.retry = int(time.Minute.Seconds() - now.Sub(w.start).Seconds() + 1)
		return e
	}
	return nil
}

// sweep drops what expired, every interval.
func (g *gateway) sweep(every time.Duration) {
	for range time.Tick(every) {
		g.prune()
	}
}

func (g *gateway) prune() {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, p := range g.pairings {
		if now.After(p.expires) {
			delete(g.pairings, k)
		}
	}
	for k, n := range g.nonces {
		if now.After(n.expires) {
			delete(g.nonces, k)
		}
	}
	for k, s := range g.sessions {
		if now.After(s.expires) {
			delete(g.sessions, k)
		}
	}
	for k, w := range g.windows {
		if now.Sub(w.start) >= time.Minute {
			delete(g.windows, k)
		}
	}
}

// --- the core ---

// call makes one typed call to the core: method, a path built here, a body encoded here.
func (g *gateway) call(ctx context.Context, method, path string, body any, device string, r *http.Request) (int, []byte, *apiError) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fail(500, "internal", "could not encode the call")
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.cfg.CoreURL+path, rd)
	if err != nil {
		return 0, nil, fail(500, "internal", "bad call")
	}
	req.Header.Set("Authorization", "Bearer "+g.cfg.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if device != "" {
		req.Header.Set("X-Stormo-Device", device)
		if r != nil {
			req.Header.Set("X-Stormo-Client-Ip", g.clientIP(r))
			if c := g.country(r); c != "" {
				req.Header.Set("X-Stormo-Client-Country", c)
			}
		}
	}
	res, err := g.core.Do(req)
	if err != nil {
		return 0, nil, fail(503, "core_unreachable", "the core did not answer; retry later")
	}
	defer res.Body.Close()
	out, err := io.ReadAll(io.LimitReader(res.Body, maxCoreAnswer))
	if err != nil {
		return 0, nil, fail(503, "core_unreachable", "the core's answer was cut")
	}
	if res.StatusCode == 401 && !bytes.Contains(out, []byte(`"device_unknown"`)) {
		// The core refused our own key: asleep and woken again, or restarted.
		return 0, nil, fail(503, "core_unreachable", "the gateway's key is no longer valid; it restarts with a new one")
	}
	return res.StatusCode, out, nil
}

// relay answers the device with the core's status and JSON (the core is trusted; its body is never
// the device's input).
func relay(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// --- routes ---

var (
	deviceID     = regexp.MustCompile(`^dev_[0-9a-f]{16}$`)
	agentID      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	conversation = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	turnID       = regexp.MustCompile(`^trn_[0-9a-f]{16}$`)
	reviewID     = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,200}$`)
	b64url       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)
	version      = regexp.MustCompile(`^[0-9a-f]{16}$`)

	convsPath = regexp.MustCompile(`^/device/v1/agents/([^/]+)/conversations$`)
	convPath  = regexp.MustCompile(`^/device/v1/agents/([^/]+)/conversations/([^/]+)$`)
	chatPath  = regexp.MustCompile(`^/device/v1/agents/([^/]+)/chat$`)
	turnPath  = regexp.MustCompile(`^/device/v1/turns/([^/]+)$`)
)

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("device gateway: internal error on %s", r.URL.Path)
			writeErr(w, fail(500, "internal", "internal error"))
		}
	}()
	path := r.URL.Path
	if path == "/device/v1/health" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	if !strings.HasPrefix(path, "/device/v1/") || (r.Method != http.MethodGet && r.Method != http.MethodPost) {
		writeErr(w, fail(404, "not_found", "no such route"))
		return
	}
	ip := g.clientIP(r)
	if e := g.allow("ip:"+ip, 120); e != nil {
		writeErr(w, e)
		return
	}
	var e *apiError
	switch {
	case path == "/device/v1/pair" && r.Method == http.MethodPost:
		e = g.pair(w, r, ip)
	case path == "/device/v1/pair/poll" && r.Method == http.MethodPost:
		e = g.poll(w, r, ip)
	case path == "/device/v1/session/challenge" && r.Method == http.MethodPost:
		e = g.challenge(w, r)
	case path == "/device/v1/session" && r.Method == http.MethodPost:
		e = g.session(w, r)
	default:
		e = g.authed(w, r)
	}
	if e != nil {
		writeErr(w, e)
	}
}

func (g *gateway) pair(w http.ResponseWriter, r *http.Request, ip string) *apiError {
	if e := g.allow("pair:"+ip, 10); e != nil {
		return e
	}
	var req struct {
		Name        string          `json:"name"`
		Model       string          `json:"model"`
		OS          string          `json:"os"`
		AppVersion  string          `json:"appVersion"`
		PublicKey   json.RawMessage `json:"publicKey"`
		Attestation []string        `json:"attestation"`
	}
	if e := decode(r, maxPairBody, &req); e != nil {
		return e
	}
	key, e := parseJWK(req.PublicKey)
	if e != nil {
		return e
	}
	if req.Attestation == nil {
		req.Attestation = []string{}
	}
	status, body, e := g.call(r.Context(), http.MethodPost, "/api/ingress/pairings", map[string]any{"name": req.Name, "model": req.Model, "os": req.OS,
		"appVersion": req.AppVersion, "publicKey": key, "attestation": req.Attestation, "ip": ip, "country": g.country(r)}, "", nil)
	if e != nil {
		return e
	}
	if status != 201 {
		relay(w, status, body)
		return nil
	}
	var p struct {
		Pairing     string `json:"pairing"`
		UserCode    string `json:"userCode"`
		Fingerprint string `json:"fingerprint"`
		ExpiresIn   int    `json:"expiresIn"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.Pairing == "" {
		return fail(503, "core_unreachable", "the core's answer was not understood")
	}
	code := random(32)
	now := g.now()
	g.mu.Lock()
	g.evict(ip)
	g.pairings[hash(code)] = &pairing{core: p.Pairing, ip: ip, created: now, expires: now.Add(time.Duration(p.ExpiresIn) * time.Second), interval: pollInterval}
	g.mu.Unlock()
	writeJSON(w, 200, map[string]any{"deviceCode": code, "userCode": p.UserCode, "fingerprint": p.Fingerprint, "expiresIn": p.ExpiresIn,
		"interval": int(pollInterval.Seconds())})
	return nil
}

// evict keeps the pending pairings under the caps, oldest first (per address, then all); the caller
// holds g.mu.
func (g *gateway) evict(ip string) {
	oldest := func(match func(*pairing) bool) (string, int) {
		key, n := "", 0
		var at time.Time
		for k, p := range g.pairings {
			if !match(p) {
				continue
			}
			n++
			if key == "" || p.created.Before(at) {
				key, at = k, p.created
			}
		}
		return key, n
	}
	for {
		k, n := oldest(func(p *pairing) bool { return p.ip == ip })
		if n < g.cfg.PerIP {
			break
		}
		delete(g.pairings, k)
	}
	for {
		k, n := oldest(func(*pairing) bool { return true })
		if n < g.cfg.MaxPending {
			break
		}
		delete(g.pairings, k)
	}
}

func (g *gateway) poll(w http.ResponseWriter, r *http.Request, ip string) *apiError {
	var req struct {
		DeviceCode string `json:"deviceCode"`
	}
	if e := decode(r, maxBody, &req); e != nil {
		return e
	}
	if !b64url.MatchString(req.DeviceCode) {
		return fail(400, "expired_token", "unknown or expired device code")
	}
	k, now := hash(req.DeviceCode), g.now()
	g.mu.Lock()
	p := g.pairings[k]
	if p == nil || now.After(p.expires) {
		delete(g.pairings, k)
		g.mu.Unlock()
		return fail(400, "expired_token", "unknown or expired device code")
	}
	if !p.lastPoll.IsZero() && now.Sub(p.lastPoll) < p.interval-time.Second {
		p.interval += 5 * time.Second
		p.lastPoll = now
		g.mu.Unlock()
		return fail(400, "slow_down", "poll less often")
	}
	p.lastPoll = now
	id := p.core
	g.mu.Unlock()
	status, body, e := g.call(r.Context(), http.MethodGet, "/api/ingress/pairings/"+url.PathEscape(id), nil, "", nil)
	if e != nil {
		return e
	}
	var st struct {
		State    string `json:"state"`
		DeviceID string `json:"deviceId"`
		Name     string `json:"name"`
	}
	if status != 200 || json.Unmarshal(body, &st) != nil {
		return fail(503, "core_unreachable", "the core's answer was not understood")
	}
	switch st.State {
	case "pending":
		return fail(400, "authorization_pending", "not approved yet")
	case "approved":
		g.mu.Lock()
		delete(g.pairings, k)
		g.mu.Unlock()
		writeJSON(w, 200, map[string]string{"deviceId": st.DeviceID, "name": st.Name})
		return nil
	case "denied":
		g.mu.Lock()
		delete(g.pairings, k)
		g.mu.Unlock()
		return fail(400, "access_denied", "the owner refused this device")
	}
	g.mu.Lock()
	delete(g.pairings, k)
	g.mu.Unlock()
	return fail(400, "expired_token", "the code expired or was used")
}

func (g *gateway) challenge(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		DeviceID string `json:"deviceId"`
	}
	if e := decode(r, maxBody, &req); e != nil {
		return e
	}
	if !deviceID.MatchString(req.DeviceID) {
		return fail(401, "device_unknown", "this device is not linked")
	}
	if e := g.allow("session:"+req.DeviceID, 10); e != nil {
		return e
	}
	// Only a linked device gets a challenge: no state for made-up ids.
	status, body, e := g.call(r.Context(), http.MethodGet, "/api/ingress/devices/"+req.DeviceID, nil, "", nil)
	if e != nil {
		return e
	}
	if status != 200 {
		if status == 401 {
			g.dropDevice(req.DeviceID) // unlinked: confirmed by the core, not a passing error
		}
		relay(w, status, body)
		return nil
	}
	n, now := random(32), g.now()
	g.mu.Lock()
	if _, has := g.nonces[req.DeviceID]; !has && len(g.nonces) >= maxNonces {
		for k, o := range g.nonces {
			if now.After(o.expires) {
				delete(g.nonces, k)
			}
		}
		if len(g.nonces) >= maxNonces {
			g.mu.Unlock()
			e := fail(429, "rate_limited", "too many sessions opening at once; retry shortly")
			e.retry = 5
			return e
		}
	}
	g.nonces[req.DeviceID] = nonce{value: n, expires: now.Add(nonceTTL)} // one at a time: a new one replaces the last
	g.mu.Unlock()
	writeJSON(w, 200, map[string]any{"nonce": n, "expiresIn": int(nonceTTL.Seconds())})
	return nil
}

// SessionMessage is what a device signs to open a session.
func sessionMessage(device, nonce string) []byte {
	return []byte("stormo-device-session\n" + device + "\n" + nonce)
}

func (g *gateway) session(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		DeviceID  string `json:"deviceId"`
		Nonce     string `json:"nonce"`
		Signature string `json:"signature"`
	}
	if e := decode(r, maxBody, &req); e != nil {
		return e
	}
	if !deviceID.MatchString(req.DeviceID) {
		return fail(401, "device_unknown", "this device is not linked")
	}
	if e := g.allow("session:"+req.DeviceID, 10); e != nil {
		return e
	}
	g.mu.Lock()
	n, ok := g.nonces[req.DeviceID]
	delete(g.nonces, req.DeviceID) // single use, whatever happens next
	g.mu.Unlock()
	if !ok || g.now().After(n.expires) || subtle.ConstantTimeCompare([]byte(n.value), []byte(req.Nonce)) != 1 {
		return fail(400, "expired_nonce", "ask for a new challenge")
	}
	sig, err := base64.RawURLEncoding.DecodeString(req.Signature)
	if err != nil || len(sig) == 0 || len(sig) > 80 {
		return fail(401, "bad_signature", "the signature is base64url DER")
	}
	status, body, e := g.call(r.Context(), http.MethodGet, "/api/ingress/devices/"+req.DeviceID, nil, "", nil)
	if e != nil {
		return e
	}
	if status != 200 {
		if status == 401 {
			g.dropDevice(req.DeviceID)
		}
		relay(w, status, body)
		return nil
	}
	var dv struct {
		PublicKey json.RawMessage `json:"publicKey"`
	}
	if err := json.Unmarshal(body, &dv); err != nil {
		return fail(503, "core_unreachable", "the core's answer was not understood")
	}
	pub, e := parseJWK(dv.PublicKey)
	if e != nil {
		return fail(503, "core_unreachable", "the core holds a key this gateway cannot read")
	}
	digest := sha256.Sum256(sessionMessage(req.DeviceID, req.Nonce))
	if !ecdsa.VerifyASN1(pub.key, digest[:], sig) {
		return fail(401, "bad_signature", "the signature does not verify with the linked key")
	}
	token := "std_" + random(32)
	expires := g.now().Add(g.cfg.TokenTTL)
	g.mu.Lock()
	g.capSessions(req.DeviceID)
	g.sessions[hash(token)] = session{device: req.DeviceID, expires: expires}
	g.mu.Unlock()
	writeJSON(w, 200, map[string]any{"token": token, "expiresIn": int(g.cfg.TokenTTL.Seconds())})
	return nil
}

// capSessions keeps a device to its newest sessions; the caller holds g.mu.
func (g *gateway) capSessions(device string) {
	for {
		key, n := "", 0
		var at time.Time
		for k, s := range g.sessions {
			if s.device == device {
				n++
				if key == "" || s.expires.Before(at) {
					key, at = k, s.expires
				}
			}
		}
		if n < sessionsPerDev {
			return
		}
		delete(g.sessions, key)
	}
}

func (g *gateway) dropDevice(device string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, s := range g.sessions {
		if s.device == device {
			delete(g.sessions, k)
		}
	}
	delete(g.nonces, device)
}

// authed serves the routes of a linked device with an open session.
func (g *gateway) authed(w http.ResponseWriter, r *http.Request) *apiError {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || !strings.HasPrefix(token, "std_") || len(token) > 100 {
		if !knownRoute(r) {
			return fail(404, "not_found", "no such route")
		}
		return fail(401, "unauthorized", "open a session (POST /device/v1/session)")
	}
	g.mu.Lock()
	s, found := g.sessions[hash(token)]
	g.mu.Unlock()
	if !found || g.now().After(s.expires) {
		return fail(401, "unauthorized", "the session expired: open a new one")
	}
	if e := g.allow("device:"+s.device, 60); e != nil {
		return e
	}
	path, ctx := r.URL.Path, r.Context()
	var (
		status int
		body   []byte
		e      *apiError
	)
	switch {
	case path == "/device/v1/me" && r.Method == http.MethodGet:
		status, body, e = g.call(ctx, http.MethodGet, "/api/ingress/me", nil, s.device, r)
		if e == nil && status == 200 {
			var m map[string]any
			if json.Unmarshal(body, &m) == nil {
				m["tokenExpires"] = s.expires.UnixMilli()
				body, _ = json.Marshal(m)
			}
		}
	case path == "/device/v1/fleet" && r.Method == http.MethodGet:
		status, body, e = g.call(ctx, http.MethodGet, "/api/ingress/fleet", nil, s.device, r)
	case path == "/device/v1/review" && r.Method == http.MethodGet:
		status, body, e = g.call(ctx, http.MethodGet, "/api/ingress/review", nil, s.device, r)
	case path == "/device/v1/review" && r.Method == http.MethodPost:
		var req struct {
			Agent    string `json:"agent"`
			Kind     string `json:"kind"`
			ID       string `json:"id"`
			Version  string `json:"version"`
			Decision string `json:"decision"`
		}
		if e := decode(r, maxBody, &req); e != nil {
			return e
		}
		if !agentID.MatchString(req.Agent) || (req.Kind != "lesson" && req.Kind != "setting" && req.Kind != "skill" && req.Kind != "schedules") ||
			!reviewID.MatchString(req.ID) || !version.MatchString(req.Version) || (req.Decision != "accept" && req.Decision != "reject") {
			return fail(400, "bad_request", "body is {agent, kind, id, version, decision: accept | reject}")
		}
		status, body, e = g.call(ctx, http.MethodPost, "/api/ingress/review", req, s.device, r)
	case convsPath.MatchString(path) && r.Method == http.MethodGet:
		a := convsPath.FindStringSubmatch(path)[1]
		if !agentID.MatchString(a) {
			return fail(404, "not_found", "no such agent")
		}
		status, body, e = g.call(ctx, http.MethodGet, "/api/ingress/agents/"+a+"/conversations", nil, s.device, r)
	case convPath.MatchString(path) && r.Method == http.MethodGet:
		m := convPath.FindStringSubmatch(path)
		if !agentID.MatchString(m[1]) || !conversation.MatchString(m[2]) {
			return fail(404, "not_found", "no such conversation")
		}
		q := ""
		if l := r.URL.Query().Get("limit"); l != "" {
			n, err := strconv.Atoi(l)
			if err != nil || n < 1 || n > 100 {
				return fail(400, "bad_request", "limit is 1 to 100")
			}
			q = "?limit=" + strconv.Itoa(n)
		}
		status, body, e = g.call(ctx, http.MethodGet, "/api/ingress/agents/"+m[1]+"/conversations/"+url.PathEscape(m[2])+q, nil, s.device, r)
	case chatPath.MatchString(path) && r.Method == http.MethodPost:
		a := chatPath.FindStringSubmatch(path)[1]
		if !agentID.MatchString(a) {
			return fail(404, "not_found", "no such agent")
		}
		var req struct {
			Conversation string `json:"conversation,omitempty"`
			Message      string `json:"message"`
		}
		if e := decode(r, maxBody, &req); e != nil {
			return e
		}
		if (req.Conversation != "" && !conversation.MatchString(req.Conversation)) || strings.TrimSpace(req.Message) == "" || len([]rune(req.Message)) > 4000 {
			return fail(400, "bad_request", "body is {conversation?, message (1 to 4000 characters)}")
		}
		status, body, e = g.call(ctx, http.MethodPost, "/api/ingress/agents/"+a+"/chat", req, s.device, r)
	case turnPath.MatchString(path) && r.Method == http.MethodGet:
		t := turnPath.FindStringSubmatch(path)[1]
		if !turnID.MatchString(t) {
			return fail(404, "not_found", "no such turn")
		}
		status, body, e = g.call(ctx, http.MethodGet, "/api/ingress/turns/"+t, nil, s.device, r)
	default:
		return fail(404, "not_found", "no such route")
	}
	if e != nil {
		return e
	}
	if status == 401 {
		g.dropDevice(s.device) // revoked or expired: its sessions end here too
	}
	relay(w, status, body)
	return nil
}

func knownRoute(r *http.Request) bool {
	p := r.URL.Path
	return p == "/device/v1/me" || p == "/device/v1/fleet" || p == "/device/v1/review" || convsPath.MatchString(p) || convPath.MatchString(p) ||
		chatPath.MatchString(p) || turnPath.MatchString(p)
}

// --- keys ---

type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

type deviceKey struct {
	jwk
	key *ecdsa.PublicKey
}

// MarshalJSON sends the key on as its canonical JWK.
func (k deviceKey) MarshalJSON() ([]byte, error) { return json.Marshal(k.jwk) }

func parseJWK(raw json.RawMessage) (deviceKey, *apiError) {
	var j jwk
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	bad := fail(400, "bad_request", "publicKey is an EC P-256 JWK {kty, crv, x, y}")
	if err := dec.Decode(&j); err != nil || j.Kty != "EC" || j.Crv != "P-256" {
		return deviceKey{}, bad
	}
	x, err1 := base64.RawURLEncoding.DecodeString(j.X)
	y, err2 := base64.RawURLEncoding.DecodeString(j.Y)
	if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
		return deviceKey{}, bad
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return deviceKey{}, bad
	}
	return deviceKey{jwk: jwk{Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString(x), Y: base64.RawURLEncoding.EncodeToString(y)}, key: pub}, nil
}
