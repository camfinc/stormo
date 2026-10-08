package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OpenAI-compatible model gateway for local agents, backed by the core's one ChatGPT sign-in
// ("Sign in with ChatGPT" plan usage, which covers open-source and locally hosted apps). LOCAL
// ONLY: a paid or remotely hosted app needs OpenAI's interest form; agents on AWS use API keys.
// Never logged: tokens, prompt or completion bodies. Logged: agent, model, status, timing, tokens.

// ModelInfo is one served model.
type ModelInfo struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	// From the catalog when it publishes one; else the default below.
	ContextLength int `json:"context_length"`
}

// DefaultModels are served before sign-in, and the context-length fallback. 272,000 is what the
// ChatGPT route enforced for gpt-6-luna under the Codex login (Hermes' table); unconfirmed for plan
// usage until the live catalog says otherwise.
var DefaultModels = []ModelInfo{{ID: "gpt-6-luna", ContextLength: 272_000}}

const (
	defaultContext = 272_000
	catalogTTL     = 10 * time.Minute
)

// AgentUsage is one agent's call counters (the office UI reads these field names).
type AgentUsage struct {
	Requests     int    `json:"requests"`
	OK           int    `json:"ok"`
	Errors       int    `json:"errors"`
	RateLimited  int    `json:"rateLimited"`
	InputTokens  int    `json:"inputTokens"`
	CachedTokens int    `json:"cachedTokens"`
	OutputTokens int    `json:"outputTokens"`
	LastAt       string `json:"lastAt,omitempty"`
	LastStatus   int    `json:"lastStatus,omitempty"`
}

// KeyResolver tells which agent a bearer header belongs to (core.AgentKeys).
type KeyResolver interface {
	AgentFor(authorization string) (string, bool)
}

// GatewayOptions configure a Gateway; zero values take the defaults.
type GatewayOptions struct {
	Auth    *ChatGPTAuth
	Keys    KeyResolver
	Client  *http.Client
	BaseURL string
	Models  []ModelInfo
	// Concurrent upstream calls across the whole fleet (one plan is shared).
	Concurrency  int
	QueueTimeout time.Duration
	// Until the upstream answers with headers.
	HeadersTimeout time.Duration
	// Longest silence inside a stream.
	IdleTimeout time.Duration
	// Hard cap on one call.
	TotalTimeout time.Duration
	// Plan-limit errors carry no reset time; hold the plan off (answering 429) this long.
	PlanHoldSeconds int
	Log             func(line string)
}

type semaphore struct {
	mu     sync.Mutex
	max    int
	active int
	queue  []chan struct{}
}

func (s *semaphore) acquire(ctx context.Context, timeout time.Duration) bool {
	s.mu.Lock()
	if s.active < s.max {
		s.active++
		s.mu.Unlock()
		return true
	}
	if len(s.queue) >= s.max*4 {
		s.mu.Unlock()
		return false
	}
	grant := make(chan struct{})
	s.queue = append(s.queue, grant)
	s.mu.Unlock()
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-grant:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, g := range s.queue {
		if g == grant {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return false
		}
	}
	// Granted while timing out: keep the slot accounting right by releasing it.
	s.releaseLocked()
	return false
}

func (s *semaphore) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked()
}

func (s *semaphore) releaseLocked() {
	if len(s.queue) > 0 {
		g := s.queue[0]
		s.queue = s.queue[1:]
		close(g) // the slot passes to the waiter; active stays the same
		return
	}
	s.active--
}

func (s *semaphore) counts() (active, queued int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, len(s.queue)
}

func writeJSON(w http.ResponseWriter, status int, body any, headers map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(marshalCompact(body))
}

func sseLine(v any) []byte { return append(append([]byte("data: "), marshalCompact(v)...), '\n', '\n') }

// Gateway serves /v1/* to local agents.
type Gateway struct {
	o         GatewayOptions
	Reasoning *ReasoningCache
	sem       *semaphore

	mu sync.Mutex
	// While the plan limit holds, answer 429 at once instead of spending calls to learn it again.
	limitedUntil time.Time
	usage        map[string]*AgentUsage
	// Calls holding a concurrency slot right now, per agent (the office UI shows them as "thinking").
	active          map[string]int
	catalog         []ModelInfo
	catalogAt       time.Time
	catalogFailedAt time.Time
}

func NewGateway(o GatewayOptions) *Gateway {
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.BaseURL == "" {
		o.BaseURL = SIWC.Resource
	}
	if o.Models == nil {
		o.Models = DefaultModels
	}
	if o.Concurrency == 0 {
		o.Concurrency = 3
	}
	if o.QueueTimeout == 0 {
		o.QueueTimeout = 30 * time.Second
	}
	if o.HeadersTimeout == 0 {
		o.HeadersTimeout = 60 * time.Second
	}
	if o.IdleTimeout == 0 {
		o.IdleTimeout = 120 * time.Second
	}
	if o.TotalTimeout == 0 {
		o.TotalTimeout = 15 * time.Minute
	}
	if o.PlanHoldSeconds == 0 {
		if n, err := strconv.Atoi(os.Getenv("SWARM_CORE_PLAN_HOLD_SECONDS")); err == nil && n > 0 {
			o.PlanHoldSeconds = n
		} else {
			o.PlanHoldSeconds = 900
		}
	}
	if o.Log == nil {
		o.Log = func(l string) { fmt.Println(l) }
	}
	return &Gateway{o: o, Reasoning: NewReasoningCache(), sem: &semaphore{max: o.Concurrency}, usage: map[string]*AgentUsage{}, active: map[string]int{}}
}

// Status is what /api/gateway serves and /api/fleet embeds; field names are read by the office UI.
type Status struct {
	Login            LoginState            `json:"login"`
	Account          *string               `json:"account"`
	ManageUsageURL   string                `json:"manageUsageUrl"`
	PlanLimitedUntil *string               `json:"planLimitedUntil"`
	Inflight         int                   `json:"inflight"`
	Queued           int                   `json:"queued"`
	Concurrency      int                   `json:"concurrency"`
	Models           []ModelInfo           `json:"models"`
	Usage            map[string]AgentUsage `json:"usage"`
	Active           map[string]int        `json:"active"`
}

func (g *Gateway) Status() Status {
	s := Status{Login: g.o.Auth.State(), ManageUsageURL: SIWC.ManageUsageURL, Concurrency: g.sem.max}
	if a := g.o.Auth.Account(); a != nil && a.Email != "" {
		s.Account = &a.Email
	}
	s.Inflight, s.Queued = g.sem.counts()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limitedUntil.After(time.Now()) {
		t := g.limitedUntil.UTC().Format("2006-01-02T15:04:05.000Z")
		s.PlanLimitedUntil = &t
	}
	s.Models = g.o.Models
	if g.catalog != nil {
		s.Models = g.catalog
	}
	s.Usage = map[string]AgentUsage{}
	for k, v := range g.usage {
		s.Usage[k] = *v
	}
	s.Active = maps.Clone(g.active)
	return s
}

// Handle serves /v1/*; it returns false for paths it does not own.
func (g *Gateway) Handle(w http.ResponseWriter, r *http.Request) bool {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/v1/") {
		return false
	}
	agent, ok := g.o.Keys.AgentFor(r.Header.Get("Authorization"))
	if !ok {
		writeJSON(w, 401, map[string]any{"error": map[string]any{"type": "invalid_api_key", "code": "invalid_api_key", "message": "unknown or missing core key"}}, nil)
		return true
	}
	switch {
	case r.Method == http.MethodGet && path == "/v1/models":
		data := []any{}
		for _, m := range g.Models(r.Context()) {
			item := map[string]any{"id": m.ID, "object": "model", "created": 0, "owned_by": "chatgpt-plan", "context_length": m.ContextLength}
			if m.DisplayName != "" {
				item["display_name"] = m.DisplayName
			}
			data = append(data, item)
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data}, nil)
	case r.Method == http.MethodPost && path == "/v1/chat/completions":
		g.chat(w, r, agent)
	default:
		writeJSON(w, 404, map[string]any{"error": map[string]any{"type": "not_found", "code": "not_found", "message": fmt.Sprintf("%s %s is not served by the core", r.Method, path)}}, nil)
	}
	return true
}

// ServeHTTP is Handle with a 404 for other paths.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !g.Handle(w, r) {
		http.NotFound(w, r)
	}
}

// Models is the signed-in plan's model catalog (`visibility: "list"`, server order), cached for
// 10 min; the configured defaults before sign-in or when the catalog is unreachable.
func (g *Gateway) Models(ctx context.Context) []ModelInfo {
	g.mu.Lock()
	if g.catalog != nil && time.Since(g.catalogAt) < catalogTTL {
		c := g.catalog
		g.mu.Unlock()
		return c
	}
	failedRecently := time.Since(g.catalogFailedAt) < time.Minute
	g.mu.Unlock()
	if g.o.Auth.State() != LoginOK || failedRecently {
		return g.o.Models
	}
	fail := func() []ModelInfo {
		g.mu.Lock()
		g.catalogFailedAt = time.Now()
		g.mu.Unlock()
		return g.o.Models
	}
	token, err := g.o.Auth.AccessToken(ctx)
	if err != nil {
		return fail()
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, g.o.BaseURL+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	res, err := g.o.Client.Do(req)
	if err != nil {
		return fail()
	}
	defer res.Body.Close()
	var body struct {
		Models []map[string]any `json:"models"`
	}
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 || json.Unmarshal(raw, &body) != nil || body.Models == nil {
		return fail()
	}
	known := map[string]int{}
	for _, m := range g.o.Models {
		known[m.ID] = m.ContextLength
	}
	models := []ModelInfo{}
	for _, m := range body.Models {
		slug, _ := m["slug"].(string)
		if m["visibility"] != "list" || slug == "" {
			continue
		}
		info := ModelInfo{ID: slug}
		if dn, ok := m["display_name"].(string); ok {
			info.DisplayName = dn
		}
		for _, k := range []string{"context_window", "max_context_window", "context_length"} {
			if n, ok := num(m[k]); ok && m[k] != nil && n > 0 && !math.IsInf(n, 0) {
				info.ContextLength = int(n)
				break
			}
		}
		if info.ContextLength == 0 {
			info.ContextLength = known[slug]
		}
		if info.ContextLength == 0 {
			info.ContextLength = defaultContext
		}
		models = append(models, info)
	}
	if len(models) == 0 {
		return fail()
	}
	g.mu.Lock()
	g.catalog, g.catalogAt = models, time.Now()
	g.mu.Unlock()
	return models
}

func (g *Gateway) record(agent string, status int, m *StreamMapper) {
	g.mu.Lock()
	defer g.mu.Unlock()
	u := g.usage[agent]
	if u == nil {
		u = &AgentUsage{}
		g.usage[agent] = u
	}
	u.Requests++
	switch status {
	case 200:
		u.OK++
	case 429:
		u.RateLimited++
	default:
		u.Errors++
	}
	if m != nil && m.Usage != nil {
		u.InputTokens += m.Usage.PromptTokens
		if m.Usage.PromptTokensDetails != nil {
			u.CachedTokens += m.Usage.PromptTokensDetails.CachedTokens
		}
		u.OutputTokens += m.Usage.CompletionTokens
	}
	u.LastAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	u.LastStatus = status
}

func isoNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func (g *Gateway) fail(w http.ResponseWriter, agent, model string, started time.Time, f UpstreamFailure) {
	if f.Status == 429 && f.Code == "usage_limit_reached" {
		if f.RetryAfterSeconds == 0 {
			f.RetryAfterSeconds = g.o.PlanHoldSeconds
		}
		g.mu.Lock()
		if until := time.Now().Add(time.Duration(f.RetryAfterSeconds) * time.Second); until.After(g.limitedUntil) {
			g.limitedUntil = until
		}
		g.mu.Unlock()
	}
	if f.Code == "plan_invalid_user" {
		g.o.Auth.MarkRelogin()
	}
	g.record(agent, f.Status, nil)
	line := fmt.Sprintf("%s llm agent=%s model=%s status=%d code=%s ms=%d", isoNow(), agent, model, f.Status, f.Code, time.Since(started).Milliseconds())
	if f.Code == "usage_limit_reached" {
		line += fmt.Sprintf(" hold=%ds manage usage: %s", f.RetryAfterSeconds, SIWC.ManageUsageURL)
	}
	g.o.Log(line)
	WriteError(w, f)
}

// Causes a call is aborted with; abortFailure maps them to what the agent sees.
var (
	errHeaders = errors.New("headers")
	errIdle    = errors.New("idle")
	errTotal   = errors.New("total")
)

func abortFailure(err error, ctx context.Context) UpstreamFailure {
	if cause := context.Cause(ctx); cause != nil {
		for _, c := range []error{errHeaders, errIdle, errTotal} {
			if errors.Is(cause, c) {
				return UpstreamFailure{Status: 504, Code: "upstream_timeout_" + c.Error(), Message: "upstream timed out (" + c.Error() + ")"}
			}
		}
		return UpstreamFailure{Status: 499, Code: "client_closed", Message: "client closed the request"}
	}
	name := "error"
	if err != nil {
		name = fmt.Sprintf("%T", err)
	}
	return UpstreamFailure{Status: 502, Code: "upstream_unreachable", Message: "upstream request failed: " + name}
}

func authFailure(err error) UpstreamFailure {
	var ae *AuthError
	if errors.As(err, &ae) {
		switch ae.Kind {
		case kindMissing:
			return UpstreamFailure{Status: 401, Code: "core_login_missing", Message: ae.Msg}
		case kindRelogin:
			return UpstreamFailure{Status: 401, Code: "core_login_expired", Message: ae.Msg}
		case kindRateLimit:
			return UpstreamFailure{Status: 429, Code: "rate_limit_exceeded", Message: ae.Msg, RetryAfterSeconds: ae.RetryAfterSeconds}
		}
		return UpstreamFailure{Status: 502, Code: "core_refresh_failed", Message: ae.Msg}
	}
	return UpstreamFailure{Status: 502, Code: "core_auth_error", Message: err.Error()}
}

// upstream makes one call with a fresh (or, after a 401, force-refreshed) access token.
func (g *Gateway) upstream(ctx context.Context, cancel context.CancelCauseFunc, body []byte, afterUnauthorized bool) (*http.Response, *UpstreamFailure) {
	token, err := g.o.Auth.AccessToken(ctx)
	if err == nil && afterUnauthorized {
		token, err = g.o.Auth.Refresh(ctx, token)
	}
	if err != nil {
		f := authFailure(err)
		return nil, &f
	}
	headers := time.AfterFunc(g.o.HeadersTimeout, func() { cancel(errHeaders) })
	defer headers.Stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.o.BaseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		f := abortFailure(err, ctx)
		return nil, &f
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	res, err := g.o.Client.Do(req)
	if err != nil {
		f := abortFailure(err, ctx)
		return nil, &f
	}
	return res, nil
}

func (g *Gateway) chat(w http.ResponseWriter, r *http.Request, agent string) {
	started := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	var shape map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &shape) != nil {
		g.fail(w, agent, "?", started, UpstreamFailure{Status: 400, Code: "invalid_json", Message: "request body is not JSON"})
		return
	}
	var modelStr string
	msgs := bytes.TrimSpace(shape["messages"])
	if len(msgs) == 0 || msgs[0] != '[' || json.Unmarshal(shape["model"], &modelStr) != nil {
		g.fail(w, agent, "?", started, UpstreamFailure{Status: 400, Code: "invalid_request", Message: "model and messages are required"})
		return
	}
	var body ChatRequest
	if json.Unmarshal(raw, &body) != nil {
		g.fail(w, agent, "?", started, UpstreamFailure{Status: 400, Code: "invalid_request", Message: "model and messages are required"})
		return
	}
	model := ModelSlug(body.Model)
	served := g.Models(r.Context())
	found := false
	ids := make([]string, len(served))
	for i, m := range served {
		ids[i] = m.ID
		found = found || m.ID == model
	}
	if !found {
		g.fail(w, agent, model, started, UpstreamFailure{Status: 404, Code: "model_not_found", Message: fmt.Sprintf("model %s is not served (have: %s)", body.Model, strings.Join(ids, ", "))})
		return
	}
	g.mu.Lock()
	remaining := time.Until(g.limitedUntil)
	g.mu.Unlock()
	if remaining > 0 {
		g.fail(w, agent, model, started, UpstreamFailure{Status: 429, Code: "usage_limit_reached", Message: "plan usage limit", RetryAfterSeconds: int(math.Ceil(remaining.Seconds()))})
		return
	}
	if !g.sem.acquire(r.Context(), g.o.QueueTimeout) {
		g.record(agent, 503, nil)
		writeJSON(w, 503, map[string]any{"error": map[string]any{"type": "server_busy", "code": "core_busy", "message": "core gateway queue is full"}}, map[string]string{"Retry-After": "5"})
		return
	}

	g.mu.Lock()
	g.active[agent]++
	g.mu.Unlock()
	ctx, cancel := context.WithCancelCause(r.Context())
	total := time.AfterFunc(g.o.TotalTimeout, func() { cancel(errTotal) })
	var idleMu sync.Mutex
	var idle *time.Timer
	bumpIdle := func() {
		idleMu.Lock()
		defer idleMu.Unlock()
		if idle == nil {
			idle = time.AfterFunc(g.o.IdleTimeout, func() { cancel(errIdle) })
		} else {
			idle.Reset(g.o.IdleTimeout)
		}
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			total.Stop()
			idleMu.Lock()
			if idle != nil {
				idle.Stop()
			}
			idleMu.Unlock()
			g.sem.release()
			g.mu.Lock()
			if n := g.active[agent] - 1; n > 0 {
				g.active[agent] = n
			} else {
				delete(g.active, agent)
			}
			g.mu.Unlock()
		})
	}
	defer cancel(nil)
	defer cleanup()

	upstreamBody := marshalCompact(ToResponsesBody(&body, agent, g.Reasoning))
	res, f := g.upstream(ctx, cancel, upstreamBody, false)
	if f == nil && res.StatusCode == 401 {
		res.Body.Close()
		res, f = g.upstream(ctx, cancel, upstreamBody, true)
	}
	if f != nil {
		cleanup()
		g.fail(w, agent, model, started, *f)
		return
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		cleanup()
		g.fail(w, agent, model, started, FailureFromResponse(res))
		return
	}
	// The direct route may omit Content-Type on a valid stream; anything else is not a stream.
	if ct := strings.ToLower(strings.TrimSpace(strings.SplitN(res.Header.Get("Content-Type"), ";", 2)[0])); ct != "" && ct != "text/event-stream" {
		cleanup()
		g.fail(w, agent, model, started, UpstreamFailure{Status: 502, Code: "upstream_not_a_stream", Message: "upstream answered " + ct})
		return
	}

	mapper := NewStreamMapper(body.Model)
	bumpIdle()
	events := NewSseReader(res.Body, bumpIdle)
	// Hold the HTTP status until the first output event, so failures that arrive as stream events
	// before any output (plan limit, bad request) still reach the agent as proper 4xx/5xx.
	var pending []any
	finished := false
	var readErr error
	for !mapper.SawOutput && !mapper.Done {
		ev, err := events.Next()
		if errors.Is(err, io.EOF) {
			finished = true
			break
		} else if err != nil {
			readErr = err
			break
		}
		pending = append(pending, mapper.Map(ev)...)
	}
	if mapper.Failure != nil {
		cleanup()
		g.fail(w, agent, model, started, *mapper.Failure)
		return
	}
	if readErr != nil && !body.Stream {
		cleanup()
		g.fail(w, agent, model, started, abortFailure(readErr, ctx))
		return
	}

	finish := func() {
		g.Reasoning.Set(mapper.CallIDs, mapper.Reasoning)
		status := 200
		if mapper.Failure != nil {
			status = mapper.Failure.Status
		}
		g.record(agent, status, mapper)
		line := fmt.Sprintf("%s llm agent=%s model=%s status=%d", isoNow(), agent, model, status)
		if body.Stream {
			line += " stream"
		}
		line += fmt.Sprintf(" ms=%d", time.Since(started).Milliseconds())
		if u := mapper.Usage; u != nil {
			cached := 0
			if u.PromptTokensDetails != nil {
				cached = u.PromptTokensDetails.CachedTokens
			}
			line += fmt.Sprintf(" in=%d cached=%d out=%d", u.PromptTokens, cached, u.CompletionTokens)
		}
		if mapper.Failure != nil {
			line += " code=" + mapper.Failure.Code
		}
		g.o.Log(line)
	}

	if !body.Stream {
		for !finished {
			ev, err := events.Next()
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				cleanup()
				g.fail(w, agent, model, started, abortFailure(err, ctx))
				return
			}
			mapper.Map(ev)
		}
		cleanup()
		if mapper.Failure != nil {
			g.fail(w, agent, model, started, *mapper.Failure)
			return
		}
		if !mapper.Done {
			g.fail(w, agent, model, started, UpstreamFailure{Status: 502, Code: "upstream_incomplete", Message: "upstream stream ended early"})
			return
		}
		finish()
		writeJSON(w, 200, mapper.Completion(), nil)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	send := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			cancel(context.Canceled) // client gone
			return false
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}
	defer finish()
	for _, c := range pending {
		send(sseLine(c))
	}
	if readErr == nil && !finished {
		for {
			ev, err := events.Next()
			if errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				readErr = err
				break
			}
			for _, c := range mapper.Map(ev) {
				send(sseLine(c))
			}
			if mapper.Done {
				break
			}
		}
	}
	errEvent := func(f UpstreamFailure) []byte {
		return sseLine(map[string]any{"error": map[string]any{"type": f.Code, "code": f.Code, "message": f.Message}})
	}
	switch {
	case readErr != nil:
		f := abortFailure(readErr, ctx)
		mapper.Failure = &f
		send(errEvent(f))
	case mapper.Failure != nil:
		send(errEvent(*mapper.Failure))
	case !mapper.Done:
		mapper.Failure = &UpstreamFailure{Status: 502, Code: "upstream_incomplete", Message: "upstream stream ended early"}
		send(errEvent(*mapper.Failure))
	default:
		for _, c := range mapper.FinalChunks() {
			send(sseLine(c))
		}
		send([]byte("data: [DONE]\n\n"))
	}
	cleanup()
}
