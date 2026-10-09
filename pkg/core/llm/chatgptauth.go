package llm

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// "Sign in with ChatGPT" with ChatGPT plan usage (OpenAI's open-source / locally hosted program,
// developers.openai.com/siwc/token-sharing-open-source). The core registers its own OAuth client
// for this user, workspace and host, so it never shares a token family with the Codex CLI.
//
//   - Login: browser loopback + PKCE. First sign-in registers with client_id=dynamic_agent_client
//     and gets an issued `oaiapp_…` client id on the callback, saved before the code exchange so a
//     failed exchange never registers a second app. The host id (`urn:uuid:…`) is stable and saved first.
//   - Refresh tokens rotate (access ~1 h, refresh ~30 d). Only the core server refreshes, one at a
//     time, under a lock directory shared with `stormo core login`; the rotated token is on disk
//     (atomic rename) before it is used, and the file is re-read under the lock.
//   - Terms: tokens are stored locally and under the user's control (0600, gitignored .swarm/),
//     never copied, snapshotted, shipped to AWS or logged.

// SIWC holds the Sign in with ChatGPT constants. The app name comes from the instance.
var SIWC = struct {
	Issuer, Resource, Scope, RegistrationClientID, CallbackPath, ManageUsageURL string
	DefaultLoginPort                                                            int
}{
	Issuer:               "https://auth.openai.com",
	Resource:             "https://api.openai.com/v1",
	Scope:                "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct",
	RegistrationClientID: "dynamic_agent_client",
	CallbackPath:         "/auth/callback",
	DefaultLoginPort:     1455,
	ManageUsageURL:       "https://chatgpt.com/settings/usage",
}

// Refresh this long before the access token expires.
const refreshSkewSeconds = 120

const lockStale = 60 * time.Second

// Refresh errors after which only a new sign-in helps (errors-and-recovery).
var reloginCodes = []string{"invalid_grant", "invalid_refresh_token", "token_expired", "refresh_token_invalidated", "refresh_token_reused", "invalid_client"}

// Connection is chatgpt.json. Field names and order are the on-disk format; keep them stable so
// a sign-in survives core upgrades. expires_at and earliest_refresh_at are epoch ms.
type Connection struct {
	Version           int      `json:"version"`
	ClientID          string   `json:"client_id"`
	ExtAgentHostID    string   `json:"ext_agent_host_id"`
	Subject           string   `json:"subject"`
	Email             string   `json:"email,omitempty"`
	IDToken           string   `json:"id_token"`
	AccessToken       string   `json:"access_token"`
	RefreshToken      string   `json:"refresh_token"`
	TokenType         string   `json:"token_type"`
	ExpiresAt         float64  `json:"expires_at"`
	EarliestRefreshAt *float64 `json:"earliest_refresh_at,omitempty"`
	Scopes            []string `json:"scopes"`
	SavedAt           string   `json:"saved_at"`
}

// LoginState is "ok" | "missing" | "relogin_required".
type LoginState string

const (
	LoginOK       LoginState = "ok"
	LoginMissing  LoginState = "missing"
	LoginRelogin  LoginState = "relogin_required"
	kindMissing              = "missing"
	kindRelogin              = "relogin"
	kindRateLimit            = "rate_limited"
	kindTransient            = "transient"
)

// AuthError is a sign-in failure; Kind is missing | relogin | rate_limited | transient.
type AuthError struct {
	Msg               string
	Kind              string
	RetryAfterSeconds int
}

func (e *AuthError) Error() string { return e.Msg }

// AuthPaths are the files under <instance>/.swarm/core/auth/.
type AuthPaths struct {
	// Tokens (0600).
	Connection string
	// Stable host id and the issued client id, kept across sign-outs so re-sign-in reuses the app.
	Registration string
}

func Paths(dir string) AuthPaths {
	return AuthPaths{Connection: filepath.Join(dir, "chatgpt.json"), Registration: filepath.Join(dir, "registration.json")}
}

// Registration is registration.json.
type Registration struct {
	ExtAgentHostID string `json:"ext_agent_host_id"`
	ClientID       string `json:"client_id,omitempty"`
}

// ---- small helpers ---------------------------------------------------------------------------

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomValue() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b64url(b)
}

func uuid4() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func readJSON[T any](path string) *T {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v T
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return &v
}

// writeAtomic writes 2-space-indented JSON plus a newline, 0600 in a 0700 dir, then renames.
func writeAtomic(path string, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, b.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WithLock holds an exclusive lock (a directory, created atomically) across processes on this host.
func WithLock[T any](lockDir string, timeout time.Duration, fn func() (T, error)) (T, error) {
	var zero T
	if err := os.MkdirAll(filepath.Dir(lockDir), 0o700); err != nil {
		return zero, err
	}
	start := time.Now()
	for {
		err := os.Mkdir(lockDir, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return zero, err
		}
		if info, serr := os.Stat(lockDir); serr == nil && time.Since(info.ModTime()) > lockStale {
			_ = os.RemoveAll(lockDir)
			continue
		} else if serr != nil {
			continue // released between mkdir and stat
		}
		if time.Since(start) > timeout {
			return zero, fmt.Errorf("timed out waiting for %s", lockDir)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer os.RemoveAll(lockDir)
	return fn()
}

// JWTClaims decodes a JWT payload without verifying it.
func JWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return map[string]any{}
	}
	var c map[string]any
	if json.Unmarshal(b, &c) != nil || c == nil {
		return map[string]any{}
	}
	return c
}

// HostID is the stable host id, created (and saved) before the first sign-in.
func HostID(p AuthPaths) (string, error) {
	if reg := readJSON[Registration](p.Registration); reg != nil && reg.ExtAgentHostID != "" {
		return reg.ExtAgentHostID, nil
	}
	id := "urn:uuid:" + uuid4()
	return id, writeAtomic(p.Registration, Registration{ExtAgentHostID: id})
}

// ---- OpenID discovery and ID-token verification ----------------------------------------------

// Discovery is the issuer's OpenID configuration.
type Discovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JwksURI               string `json:"jwks_uri"`
}

// Oidc caches discovery and the issuer's signing keys.
type Oidc struct {
	client *http.Client
	Issuer string

	mu        sync.Mutex
	discovery *Discovery
	keys      []map[string]any
	keysAt    time.Time
}

func NewOidc(client *http.Client, issuer string) *Oidc {
	if client == nil {
		client = http.DefaultClient
	}
	if issuer == "" {
		issuer = SIWC.Issuer
	}
	return &Oidc{client: client, Issuer: issuer}
}

func origin(u string) string {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" {
		return ""
	}
	return p.Scheme + "://" + p.Host
}

func (o *Oidc) getJSON(ctx context.Context, u string, v any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	res, err := o.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, json.Unmarshal(b, v)
}

// Config fetches (once) and verifies the issuer's discovery document.
func (o *Oidc) Config(ctx context.Context) (*Discovery, error) {
	o.mu.Lock()
	if d := o.discovery; d != nil {
		o.mu.Unlock()
		return d, nil
	}
	o.mu.Unlock()
	var d Discovery
	status, err := o.getJSON(ctx, o.Issuer+"/.well-known/openid-configuration", &d)
	if err != nil && status == 0 {
		return nil, &AuthError{Msg: "ChatGPT sign-in configuration is unreachable: " + err.Error(), Kind: kindTransient}
	}
	same := func(u string) bool { return origin(u) != "" && origin(u) == origin(o.Issuer) }
	if err != nil || status < 200 || status > 299 || d.Issuer != o.Issuer || !same(d.AuthorizationEndpoint) || !same(d.TokenEndpoint) || !same(d.JwksURI) {
		return nil, &AuthError{Msg: "ChatGPT sign-in configuration could not be verified", Kind: kindTransient}
	}
	o.mu.Lock()
	o.discovery = &d
	o.mu.Unlock()
	return &d, nil
}

func (o *Oidc) jwks(ctx context.Context, force bool) ([]map[string]any, error) {
	o.mu.Lock()
	if !force && o.keys != nil && time.Since(o.keysAt) < time.Hour {
		k := o.keys
		o.mu.Unlock()
		return k, nil
	}
	o.mu.Unlock()
	d, err := o.Config(ctx)
	if err != nil {
		return nil, err
	}
	var body struct {
		Keys []map[string]any `json:"keys"`
	}
	status, err := o.getJSON(ctx, d.JwksURI, &body)
	if err != nil || status < 200 || status > 299 || body.Keys == nil {
		return nil, &AuthError{Msg: "ChatGPT identity keys are unavailable", Kind: kindTransient}
	}
	o.mu.Lock()
	o.keys, o.keysAt = body.Keys, time.Now()
	o.mu.Unlock()
	return body.Keys, nil
}

func decodeSeg(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

func rsaKey(jwk map[string]any) (*rsa.PublicKey, error) {
	n, err := decodeSeg(str(jwk["n"]))
	if err != nil {
		return nil, err
	}
	e, err := decodeSeg(str(jwk["e"]))
	if err != nil || len(e) == 0 || len(e) > 4 {
		return nil, errors.New("bad exponent")
	}
	exp := 0
	for _, b := range e {
		exp = exp<<8 | int(b)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, nil
}

// Identity is who an ID token names.
type Identity struct {
	Sub   string
	Email string
}

// VerifyOptions for VerifyIDToken; Nonce nil means not checked.
type VerifyOptions struct {
	Nonce        *string
	IgnoreExpiry bool
}

// VerifyIDToken checks the RS256 signature, issuer, audience (the issued client id), expiry,
// nonce and subject.
func (o *Oidc) VerifyIDToken(ctx context.Context, idToken, clientID string, vo VerifyOptions) (*Identity, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return nil, &AuthError{Msg: "invalid ID token", Kind: kindRelogin}
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	hb, err := decodeSeg(parts[0])
	if err != nil || json.Unmarshal(hb, &header) != nil {
		return nil, &AuthError{Msg: "invalid ID token", Kind: kindRelogin}
	}
	if header.Alg != "RS256" {
		return nil, &AuthError{Msg: "unexpected ID token algorithm", Kind: kindRelogin}
	}
	find := func(keys []map[string]any) map[string]any {
		for _, k := range keys {
			if str(k["kid"]) == header.Kid {
				return k
			}
		}
		return nil
	}
	keys, err := o.jwks(ctx, false)
	if err != nil {
		return nil, err
	}
	jwk := find(keys)
	if jwk == nil {
		if keys, err = o.jwks(ctx, true); err != nil {
			return nil, err
		}
		jwk = find(keys)
	}
	if jwk == nil {
		return nil, &AuthError{Msg: "ID token signing key not found", Kind: kindTransient}
	}
	pub, err := rsaKey(jwk)
	if err != nil {
		return nil, &AuthError{Msg: "ID token signature is invalid", Kind: kindRelogin}
	}
	sig, err := decodeSeg(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) != nil {
		return nil, &AuthError{Msg: "ID token signature is invalid", Kind: kindRelogin}
	}
	c := JWTClaims(idToken)
	d, err := o.Config(ctx)
	if err != nil {
		return nil, err
	}
	var aud []any
	if a, ok := c["aud"].([]any); ok {
		aud = a
	} else {
		aud = []any{c["aud"]}
	}
	azp, hasAzp := c["azp"]
	now := float64(time.Now().UnixMilli()) / 1000
	exp, expOK := num(c["exp"])
	var problems []string
	if c["iss"] != d.Issuer {
		problems = append(problems, "issuer")
	}
	if !slices.Contains(aud, any(clientID)) {
		problems = append(problems, "audience")
	}
	if hasAzp && azp != clientID {
		problems = append(problems, "azp")
	}
	if len(aud) > 1 && azp != clientID {
		problems = append(problems, "azp")
	}
	if !vo.IgnoreExpiry && !(expOK && c["exp"] != nil && exp > now-5) {
		problems = append(problems, "expired")
	}
	if vo.Nonce != nil && c["nonce"] != *vo.Nonce {
		problems = append(problems, "nonce")
	}
	sub, _ := c["sub"].(string)
	if sub == "" {
		problems = append(problems, "subject")
	}
	if len(problems) > 0 {
		return nil, &AuthError{Msg: "ID token rejected (" + strings.Join(problems, ", ") + ")", Kind: kindRelogin}
	}
	id := &Identity{Sub: sub}
	if e, ok := c["email"].(string); ok {
		id.Email = e
	}
	return id, nil
}

type tokenResponse struct {
	AccessToken       string  `json:"access_token"`
	RefreshToken      string  `json:"refresh_token"`
	IDToken           string  `json:"id_token"`
	TokenType         string  `json:"token_type"`
	ExpiresIn         float64 `json:"expires_in"`
	Scope             string  `json:"scope"`
	EarliestRefreshAt any     `json:"earliest_refresh_at"`
}

// epochMs: a number below 1e12 is seconds; a string is a date.
func epochMs(v any) *float64 {
	switch x := v.(type) {
	case float64:
		if x < 1e12 {
			x *= 1000
		}
		return &x
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02"} {
			if t, err := time.Parse(layout, x); err == nil {
				ms := float64(t.UnixMilli())
				return &ms
			}
		}
	}
	return nil
}

func errorCode(body []byte) string {
	var b struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &b) != nil || len(b.Error) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b.Error, &s) == nil {
		return s
	}
	var o struct {
		Code *string `json:"code"`
		Type *string `json:"type"`
	}
	if json.Unmarshal(b.Error, &o) == nil {
		if o.Code != nil {
			return *o.Code
		}
		if o.Type != nil {
			return *o.Type
		}
	}
	return ""
}

func isoMs(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z") }

func postForm(ctx context.Context, client *http.Client, endpoint string, form [][2]string) (*http.Response, []byte, error) {
	vals := make([]string, len(form))
	for i, kv := range form {
		vals[i] = url.QueryEscape(kv[0]) + "=" + url.QueryEscape(kv[1])
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(strings.Join(vals, "&")))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res, body, err
}

// ---- the server-side token holder ------------------------------------------------------------

// AuthOptions configure a ChatGPTAuth.
type AuthOptions struct {
	Paths  AuthPaths
	Client *http.Client
	Issuer string
	// Now is epoch ms (tests).
	Now            func() int64
	RefreshTimeout time.Duration
}

// Account is who is signed in (no tokens).
type Account struct {
	Email    string
	ClientID string
	Scopes   []string
}

// ChatGPTAuth holds the core's one ChatGPT connection.
type ChatGPTAuth struct {
	Oidc           *Oidc
	paths          AuthPaths
	lockDir        string
	client         *http.Client
	now            func() int64
	refreshTimeout time.Duration

	mu    sync.Mutex
	conn  *Connection
	mtime int64
	// The refresh token the server rejected; cleared once the file holds a different one.
	deadRefreshToken string
	inflight         *flight
}

type flight struct {
	done  chan struct{}
	token string
	err   error
}

func NewChatGPTAuth(o AuthOptions) *ChatGPTAuth {
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	now := o.Now
	if now == nil {
		now = func() int64 { return time.Now().UnixMilli() }
	}
	rt := o.RefreshTimeout
	if rt == 0 {
		rt = 20 * time.Second
	}
	return &ChatGPTAuth{Oidc: NewOidc(client, o.Issuer), paths: o.Paths, lockDir: o.Paths.Connection + ".lock", client: client, now: now, refreshTimeout: rt, mtime: -1}
}

// reload re-reads the connection when its mtime changed. Caller holds a.mu.
func (a *ChatGPTAuth) reload() {
	var m int64
	if info, err := os.Stat(a.paths.Connection); err == nil {
		m = info.ModTime().UnixNano()
	}
	if m == a.mtime {
		return
	}
	a.mtime = m
	c := readJSON[Connection](a.paths.Connection)
	if c != nil && c.AccessToken != "" && c.RefreshToken != "" && c.ClientID != "" {
		a.conn = c
	} else {
		a.conn = nil
	}
	if a.conn != nil && a.conn.RefreshToken != a.deadRefreshToken {
		a.deadRefreshToken = ""
	}
}

func (a *ChatGPTAuth) State() LoginState {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reload()
	if a.conn == nil {
		return LoginMissing
	}
	if a.deadRefreshToken != "" && a.deadRefreshToken == a.conn.RefreshToken {
		return LoginRelogin
	}
	return LoginOK
}

func (a *ChatGPTAuth) Account() *Account {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reload()
	if a.conn == nil {
		return nil
	}
	return &Account{Email: a.conn.Email, ClientID: a.conn.ClientID, Scopes: a.conn.Scopes}
}

// MarkRelogin: the server rejected the access token's identity outright
// (e.g. `subscription_sharing_invalid_user`).
func (a *ChatGPTAuth) MarkRelogin() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reload()
	if a.conn != nil {
		a.deadRefreshToken = a.conn.RefreshToken
	}
}

func (a *ChatGPTAuth) expiring(c *Connection) bool {
	return float64(a.now()) >= c.ExpiresAt-refreshSkewSeconds*1000
}

func errMissing() error {
	return &AuthError{Msg: "core is not signed in to ChatGPT (run `stormo core login`)", Kind: kindMissing}
}

func errExpired() error {
	return &AuthError{Msg: "ChatGPT sign-in expired or was revoked (run `stormo core login`)", Kind: kindRelogin}
}

// AccessToken is a valid access token, refreshing first when it is about to expire.
func (a *ChatGPTAuth) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	a.reload()
	c := a.conn
	dead := c != nil && a.deadRefreshToken != "" && a.deadRefreshToken == c.RefreshToken
	a.mu.Unlock()
	if c == nil {
		return "", errMissing()
	}
	if dead {
		return "", errExpired()
	}
	if !a.expiring(c) {
		return c.AccessToken, nil
	}
	return a.Refresh(ctx, c.AccessToken)
}

// Refresh refreshes, one at a time. staleAccess is the token the caller saw expire or fail: if a
// refresh or a new sign-in already replaced it, that one is adopted instead of spending the
// refresh token.
func (a *ChatGPTAuth) Refresh(ctx context.Context, staleAccess string) (string, error) {
	a.mu.Lock()
	f := a.inflight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		a.inflight = f
		go func() {
			f.token, f.err = a.doRefresh(staleAccess)
			a.mu.Lock()
			a.inflight = nil
			a.mu.Unlock()
			close(f.done)
		}()
	}
	a.mu.Unlock()
	select {
	case <-f.done:
		return f.token, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (a *ChatGPTAuth) doRefresh(staleAccess string) (string, error) {
	return WithLock(a.lockDir, a.refreshTimeout+10*time.Second, func() (string, error) {
		a.mu.Lock()
		a.mtime = -1
		a.reload() // re-read under the lock
		c := a.conn
		dead := a.deadRefreshToken
		a.mu.Unlock()
		if c == nil {
			return "", errMissing()
		}
		if c.AccessToken != staleAccess && !a.expiring(c) {
			return c.AccessToken, nil
		}
		if dead != "" && dead == c.RefreshToken {
			return "", errExpired()
		}

		ctx, cancel := context.WithTimeout(context.Background(), a.refreshTimeout)
		defer cancel()
		d, err := a.Oidc.Config(ctx)
		if err != nil {
			return "", err
		}
		res, body, err := postForm(ctx, a.client, d.TokenEndpoint, [][2]string{
			{"grant_type", "refresh_token"}, {"client_id", c.ClientID}, {"refresh_token", c.RefreshToken}, {"resource", SIWC.Resource},
		})
		if err != nil {
			name := "Error"
			if errors.Is(err, context.DeadlineExceeded) {
				name = "TimeoutError"
			}
			return "", &AuthError{Msg: "token refresh failed: " + name, Kind: kindTransient}
		}
		if res.StatusCode == 429 {
			ra, _ := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After")))
			if ra <= 0 {
				ra = 60
			}
			return "", &AuthError{Msg: "token refresh rate-limited", Kind: kindRateLimit, RetryAfterSeconds: ra}
		}
		if res.StatusCode >= 500 {
			return "", &AuthError{Msg: fmt.Sprintf("token refresh failed: HTTP %d", res.StatusCode), Kind: kindTransient}
		}
		if res.StatusCode != 200 {
			code := errorCode(body)
			if code != "" && !slices.Contains(reloginCodes, code) && res.StatusCode != 400 && res.StatusCode != 401 {
				return "", &AuthError{Msg: "token refresh failed (" + code + ")", Kind: kindTransient}
			}
			a.mu.Lock()
			a.deadRefreshToken = c.RefreshToken
			a.mu.Unlock()
			what := code
			if what == "" {
				what = strconv.Itoa(res.StatusCode)
			}
			return "", &AuthError{Msg: "ChatGPT sign-in rejected on refresh (" + what + "); run `stormo core login`", Kind: kindRelogin}
		}
		var t tokenResponse
		if json.Unmarshal(body, &t) != nil || t.AccessToken == "" || t.ExpiresIn == 0 {
			return "", &AuthError{Msg: "token refresh returned incomplete credentials", Kind: kindTransient}
		}
		next := *c
		next.AccessToken = t.AccessToken
		if t.RefreshToken != "" {
			next.RefreshToken = t.RefreshToken
		}
		if t.IDToken != "" {
			next.IDToken = t.IDToken
		}
		now := a.now()
		next.ExpiresAt = float64(now) + t.ExpiresIn*1000
		next.EarliestRefreshAt = epochMs(t.EarliestRefreshAt)
		if t.Scope != "" {
			next.Scopes = strings.Fields(t.Scope)
		}
		next.SavedAt = isoMs(now)
		// Durable before anything uses it: the old refresh token is already spent.
		if err := writeAtomic(a.paths.Connection, next); err != nil {
			return "", err
		}
		a.mu.Lock()
		a.conn = &next
		if info, err := os.Stat(a.paths.Connection); err == nil {
			a.mtime = info.ModTime().UnixNano()
		}
		a.mu.Unlock()
		if t.IDToken != "" {
			// Same account as before, or the connection is not ours to use.
			who, err := a.Oidc.VerifyIDToken(ctx, t.IDToken, c.ClientID, VerifyOptions{IgnoreExpiry: true})
			if err != nil {
				var ae *AuthError
				if errors.As(err, &ae) && ae.Kind == kindTransient {
					who = &Identity{Sub: c.Subject}
				} else {
					return "", err
				}
			}
			if who.Sub != c.Subject {
				a.mu.Lock()
				a.deadRefreshToken = next.RefreshToken
				a.mu.Unlock()
				return "", &AuthError{Msg: "refreshed identity is a different ChatGPT account; run `stormo core login`", Kind: kindRelogin}
			}
		}
		return next.AccessToken, nil
	})
}

// ---- sign-in (run by `stormo core login` on the owner's machine) -----------------------------

// SignInOptions configure SignIn.
type SignInOptions struct {
	Paths  AuthPaths
	Client *http.Client
	Issuer string
	// AppName is the instance's display name, sent as the app name on first registration.
	AppName string
	// Port is the loopback port for the callback (nil: 1455); a busy port falls back to an
	// OS-assigned one.
	Port        *int
	OpenBrowser func(url string) error
	Print       func(line string)
	Timeout     time.Duration
}

// DefaultOpenBrowser opens u in the system browser (open, xdg-open, explorer).
func DefaultOpenBrowser(u string) error {
	cmd := "xdg-open"
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd = "explorer"
	}
	return exec.Command(cmd, u).Start()
}

var clientIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,200}$`)

type callbackResult struct {
	code, clientID string
	err            error
}

// SignIn runs the browser loopback + PKCE sign-in and saves the connection.
func SignIn(ctx context.Context, o SignInOptions) (*Connection, error) {
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	oidc := NewOidc(client, o.Issuer)
	provider, err := oidc.Config(ctx)
	if err != nil {
		return nil, err
	}
	host, err := HostID(o.Paths)
	if err != nil {
		return nil, err
	}
	reg := readJSON[Registration](o.Paths.Registration)
	if reg == nil {
		reg = &Registration{ExtAgentHostID: host}
	}
	previous := readJSON[Connection](o.Paths.Connection)
	state, nonce, verifier := randomValue(), randomValue(), randomValue()

	results := make(chan callbackResult, 1)
	var once sync.Once
	settle := func(r callbackResult) { once.Do(func() { results <- r }) }
	var mu sync.Mutex
	done := false
	page := func(w http.ResponseWriter, status int, msg string) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(status)
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title><body style="font:17px system-ui;max-width:32rem;margin:18vh auto;padding:24px"><h1>%s</h1><p>You can close this tab and return to the terminal.</p>`, html.EscapeString(o.AppName), msg)
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != SIWC.CallbackPath || done {
			http.Error(w, "Not found", http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		// Unrelated loopback requests must not consume the pending sign-in.
		if got := q.Get("state"); len(got) != len(state) || subtle.ConstantTimeCompare([]byte(got), []byte(state)) != 1 {
			http.Error(w, "Invalid sign-in state", http.StatusBadRequest)
			return
		}
		done = true
		if q.Has("error") {
			settle(callbackResult{err: errors.New("sign-in was not completed: " + q.Get("error"))})
			page(w, 400, "Sign-in was not completed")
			return
		}
		code := q.Get("code")
		clientID := reg.ClientID
		if q.Has("client_id") {
			clientID = q.Get("client_id")
		}
		if code == "" || clientID == "" || clientID == SIWC.RegistrationClientID || !clientIDRe.MatchString(clientID) || (reg.ClientID != "" && q.Get("client_id") != "" && q.Get("client_id") != reg.ClientID) {
			settle(callbackResult{err: errors.New("ChatGPT did not complete app registration; try again")})
			page(w, 400, "Registration did not complete")
			return
		}
		settle(callbackResult{code: code, clientID: clientID})
		page(w, 200, "Signed in to ChatGPT")
	})

	port := SIWC.DefaultLoginPort
	if o.Port != nil {
		port = *o.Port
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil { // port busy (e.g. a Codex CLI login)
			return nil, err
		}
	}
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", ln.Addr().(*net.TCPAddr).Port, SIWC.CallbackPath)

	challenge := sha256.Sum256([]byte(verifier))
	clientParam := reg.ClientID
	if clientParam == "" {
		clientParam = SIWC.RegistrationClientID
	}
	params := [][2]string{
		{"client_id", clientParam}, {"response_type", "code"}, {"redirect_uri", redirectURI}, {"scope", SIWC.Scope},
		{"resource", SIWC.Resource}, {"state", state}, {"nonce", nonce}, {"code_challenge_method", "S256"},
		{"code_challenge", b64url(challenge[:])}, {"ext_agent_host_id", host},
	}
	if reg.ClientID == "" {
		params = append(params, [2]string{"agent_name_hint", o.AppName})
	}
	if previous != nil && previous.Email != "" {
		params = append(params, [2]string{"login_hint", previous.Email})
	}
	authURL, err := url.Parse(provider.AuthorizationEndpoint)
	if err != nil {
		return nil, err
	}
	enc := make([]string, len(params))
	for i, kv := range params {
		enc[i] = url.QueryEscape(kv[0]) + "=" + url.QueryEscape(kv[1])
	}
	authURL.RawQuery = strings.Join(enc, "&")

	print := o.Print
	if print == nil {
		print = func(string) {}
	}
	print("Continue with ChatGPT: your browser is opening the sign-in page. If it does not, open:")
	print(authURL.String())
	open := o.OpenBrowser
	if open == nil {
		open = DefaultOpenBrowser
	}
	go func() { _ = open(authURL.String()) }()

	timeout := o.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	var result callbackResult
	select {
	case result = <-results:
	case <-time.After(timeout):
		return nil, errors.New("sign-in timed out")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if result.err != nil {
		return nil, result.err
	}

	// Keep the issued client id even if the one-time code exchange fails, so a retry reuses the app.
	if err := writeAtomic(o.Paths.Registration, Registration{ExtAgentHostID: host, ClientID: result.clientID}); err != nil {
		return nil, err
	}
	res, body, err := postForm(ctx, client, provider.TokenEndpoint, [][2]string{
		{"grant_type", "authorization_code"}, {"code", result.code}, {"client_id", result.clientID},
		{"code_verifier", verifier}, {"redirect_uri", redirectURI}, {"resource", SIWC.Resource},
	})
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		code := errorCode(body)
		if code == "" {
			code = fmt.Sprintf("HTTP %d", res.StatusCode)
		}
		return nil, errors.New("token exchange failed: " + code)
	}
	var t tokenResponse
	_ = json.Unmarshal(body, &t)
	scopes := strings.Fields(t.Scope)
	if t.AccessToken == "" || t.RefreshToken == "" || t.IDToken == "" || t.ExpiresIn == 0 {
		return nil, errors.New("ChatGPT returned incomplete credentials")
	}
	if !slices.Contains(scopes, "chatgpt.tokens.use.direct") {
		return nil, errors.New("ChatGPT plan usage was not granted (scope chatgpt.tokens.use.direct missing)")
	}
	who, err := oidc.VerifyIDToken(ctx, t.IDToken, result.clientID, VerifyOptions{Nonce: &nonce})
	if err != nil {
		return nil, err
	}
	if previous != nil && previous.Subject != "" && previous.ClientID == result.clientID && previous.Subject != who.Sub {
		return nil, errors.New("this sign-in returned a different ChatGPT account than the saved one; run `stormo core logout` first to switch")
	}
	now := time.Now().UnixMilli()
	conn := &Connection{
		Version: 1, ClientID: result.clientID, ExtAgentHostID: host, Subject: who.Sub, Email: who.Email,
		IDToken: t.IDToken, AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, TokenType: "Bearer",
		ExpiresAt: float64(now) + t.ExpiresIn*1000, EarliestRefreshAt: epochMs(t.EarliestRefreshAt),
		Scopes: scopes, SavedAt: isoMs(now),
	}
	if _, err := WithLock(o.Paths.Connection+".lock", 30*time.Second, func() (struct{}, error) {
		return struct{}{}, writeAtomic(o.Paths.Connection, conn)
	}); err != nil {
		return nil, err
	}
	return conn, nil
}

// SignOut forgets the tokens locally. The registration (host id, client id) is kept for the next
// sign-in.
func SignOut(p AuthPaths) (bool, error) {
	return WithLock(p.Connection+".lock", 30*time.Second, func() (bool, error) {
		if _, err := os.Stat(p.Connection); os.IsNotExist(err) {
			return false, nil
		}
		return true, os.Remove(p.Connection)
	})
}
