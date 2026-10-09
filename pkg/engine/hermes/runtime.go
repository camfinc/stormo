package hermes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
)

// APIKeyName is the secret keying Hermes' API server (per agent; `stormo secrets init` mints it).
const APIKeyName = "API_SERVER_KEY"

// SignatureHeader carries Hermes' outbound hook signature: `sha256=<hex HMAC-SHA256 of the body>`.
const SignatureHeader = "X-Hermes-Signature-256"

// Runtime is Hermes' API server (0.21+): `/health/detailed`, `/api/sessions`, `/api/jobs`,
// `POST /v1/runs`, `/api/sessions/{id}/chat`, and its outbound hooks.
func (h *Hermes) Runtime() engine.Runtime { return runtime{} }

// NewRuntime is the runtime without an instance.
func NewRuntime() engine.Runtime { return runtime{} }

type runtime struct{}

func (runtime) APIKeyName() string { return APIKeyName }

// ValidAPIKey: Hermes only serves its API with a key of 16+ characters.
func (runtime) ValidAPIKey(key string) bool { return len(key) >= 16 }

var (
	sourceRe = regexp.MustCompile(`^[a-z_-]{1,24}$`)
	jobName  = regexp.MustCompile(`[^\p{L}\p{N} ._:/()-]`)
	sigRe    = regexp.MustCompile(`^sha256=([0-9a-f]{64})$`)
)

func isoMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func (runtime) Activity(ctx context.Context, get func(ctx context.Context, path string) (any, error), state any, now time.Time) (*engine.Activity, any, error) {
	memo, _ := state.(map[string]*JobMemo)
	if memo == nil {
		memo = map[string]*JobMemo{}
	}
	type result struct {
		v   any
		err error
	}
	fetch := func(path string) chan result {
		ch := make(chan result, 1)
		go func() { v, err := get(ctx, path); ch <- result{v, err} }()
		return ch
	}
	hc, sc := fetch("/health/detailed"), fetch("/api/sessions?limit=5")
	// Scheduled jobs (no_agent scripts run outside any agent turn): optional, never fatal.
	jc := fetch("/api/jobs")
	h, s, j := <-hc, <-sc, <-jc
	if h.err != nil || s.err != nil {
		return nil, memo, errors.Join(h.err, s.err)
	}
	jobs := RunningJobs(j.v, memo, float64(now.UnixMilli()))
	act := ActivityFrom(h.v, s.v, now, jobs)
	if act != nil {
		act.LastJobAt = LastJobRun(j.v)
	}
	return act, memo, nil
}

func (runtime) WakeRequest(ctx context.Context, endpoint, input, session, idempotency string) (*http.Request, error) {
	body, _ := json.Marshal(map[string]string{"input": input, "session_id": session})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/runs", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotency)
	return req, nil
}

func (runtime) HookSignature(h http.Header) string {
	if m := sigRe.FindStringSubmatch(h.Get(SignatureHeader)); m != nil {
		return m[1]
	}
	return ""
}

// hookDelivery is the body Hermes' agent/outbound_webhooks.py posts.
type hookDelivery struct {
	Event     string         `json:"hook_event_name"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	SessionID string         `json:"session_id"`
	Extra     map[string]any `json:"extra"`
	Delivery  string         `json:"delivery_id"`
	Timestamp string         `json:"timestamp"`
}

var hookKinds = map[string]engine.HookKind{
	"on_session_start": engine.SessionStart, "on_session_end": engine.SessionEnd,
	"pre_tool_call": engine.ToolStart, "post_tool_call": engine.ToolDone,
	"pre_approval_request": engine.ApprovalWaiting, "post_approval_response": engine.ApprovalAnswered,
}

func (runtime) ParseHook(body []byte) (*engine.HookEvent, error) {
	var d hookDelivery
	if err := json.Unmarshal(body, &d); err != nil || d.Event == "" || d.Delivery == "" {
		return nil, errors.New("not a Hermes hook delivery")
	}
	at, err := time.Parse(time.RFC3339Nano, d.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("hook timestamp: %w", err)
	}
	kind, ok := hookKinds[d.Event]
	if !ok {
		kind = engine.OtherHook
	}
	e := &engine.HookEvent{Kind: kind, Name: d.Event, Delivery: d.Delivery, At: at, Session: d.SessionID,
		Tool: d.ToolName, ToolInput: d.ToolInput}
	e.CallID, _ = d.Extra["tool_call_id"].(string)
	if e.CallID == "" {
		e.CallID = d.SessionID + "\x00" + d.ToolName
	}
	for _, k := range []string{"approval_id", "request_id", "rule_key"} {
		if s, ok := d.Extra[k].(string); ok && s != "" {
			e.ApprovalID = s
			break
		}
	}
	if e.ApprovalID == "" {
		e.ApprovalID = d.SessionID
	}
	e.Platform, _ = d.Extra["platform"].(string)
	e.Status, _ = d.Extra["status"].(string)
	if ms, ok := d.Extra["duration_ms"].(float64); ok {
		e.Duration = time.Duration(ms) * time.Millisecond
	}
	return e, nil
}

// JobMemo is what RunningJobs remembers about a job between polls.
type JobMemo struct {
	Next, Last string
	Running    bool
}

func parseMs(s string) (float64, bool) {
	if s == "" {
		return math.NaN(), false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return math.NaN(), false
	}
	return float64(t.UnixMilli()), true
}

func jobList(jobs any) []any {
	m, ok := jobs.(map[string]any)
	if !ok {
		return nil
	}
	l, _ := m["jobs"].([]any)
	return l
}

// RunningJobs tells which of an agent's scheduled jobs are running, from Hermes' `/api/jobs`. It
// has no running flag, but its scheduler (v0.21) moves `next_run_at` forward when a run starts and
// sets `last_run_at` when it finishes. So a job is running while (a) for an interval job, its last
// finish is earlier than the start implied by `next_run_at - interval`, or (b) we saw `next_run_at`
// move with no new `last_run_at`. (a) also covers a core started mid-run; (b) covers cron
// expressions and first runs. memo carries what (b) needs between polls. now is epoch ms.
func RunningJobs(jobs any, memo map[string]*JobMemo, now float64) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range jobList(jobs) {
		j, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, ok := j["id"].(string)
		if !ok || j["enabled"] == false {
			continue
		}
		seen[id] = true
		next, _ := j["next_run_at"].(string)
		last, _ := j["last_run_at"].(string)
		prev := memo[id]
		running := prev != nil && prev.Running
		if prev != nil && last != prev.Last {
			running = false // finished
		} else if prev != nil && next != "" && next != prev.Next && last == prev.Last {
			running = true // started
		}
		if sch, ok := j["schedule"].(map[string]any); ok && sch["kind"] == "interval" {
			if minutes, ok := sch["minutes"].(float64); ok && next != "" && last != "" {
				nextMs, _ := parseMs(next)
				lastMs, _ := parseMs(last)
				startedAt := nextMs - minutes*60_000
				running = lastMs < startedAt-5_000 && startedAt <= now // an unparsable time is NaN, which compares false
			}
		}
		memo[id] = &JobMemo{Next: next, Last: last, Running: running}
		name := ""
		if n, ok := j["name"].(string); ok {
			r := []rune(strings.TrimSpace(jobName.ReplaceAllString(n, "")))
			if len(r) > 60 {
				r = r[:60]
			}
			name = string(r)
		}
		if running && name != "" {
			out = append(out, name)
		}
	}
	for id := range memo {
		if !seen[id] {
			delete(memo, id)
		}
	}
	return out
}

// LastJobRun is the latest `last_run_at` among an agent's scheduled jobs, or "".
func LastJobRun(jobs any) string {
	best, found := 0.0, false
	for _, raw := range jobList(jobs) {
		j, _ := raw.(map[string]any)
		s, _ := j["last_run_at"].(string)
		if t, ok := parseMs(s); ok && (!found || t > best) {
			best, found = t, true
		}
	}
	if !found {
		return ""
	}
	return isoMillis(time.UnixMilli(int64(best)))
}

// ActivityFrom reads Hermes' `/health/detailed` and `/api/sessions` bodies; nil if not usable.
func ActivityFrom(health, sessions any, now time.Time, jobs []string) *engine.Activity {
	h, ok := health.(map[string]any)
	if !ok {
		return nil
	}
	if jobs == nil {
		jobs = []string{}
	}
	a := &engine.Activity{RunningJobs: jobs, Platforms: map[string]engine.Platform{}, PolledAt: isoMillis(now)}
	if ps, ok := h["platforms"].(map[string]any); ok {
		for name, v := range ps {
			p, ok := v.(map[string]any)
			if !sourceRe.MatchString(name) || !ok {
				continue
			}
			state := "unknown"
			if s, ok := p["state"].(string); ok && sourceRe.MatchString(s) {
				state = s
			}
			a.Platforms[name] = engine.Platform{State: state, NeedsAttention: p["needs_attention"] == true}
		}
	}
	var newest map[string]any
	if s, ok := sessions.(map[string]any); ok {
		list, _ := s["data"].([]any)
		for _, raw := range list {
			x, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			la, ok := x["last_active"].(float64)
			if !ok {
				continue
			}
			if newest == nil || la > newest["last_active"].(float64) {
				newest = x
			}
		}
	}
	if n, ok := h["active_agents"].(float64); ok {
		a.ActiveAgents = int(n)
	}
	a.GatewayBusy = h["gateway_busy"] == true
	if newest != nil {
		if src, ok := newest["source"].(string); ok && sourceRe.MatchString(src) {
			a.Source = src
		}
		a.LastActive = isoMillis(time.UnixMilli(int64(newest["last_active"].(float64) * 1000)))
	}
	return a
}
