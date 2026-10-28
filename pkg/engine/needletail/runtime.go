package needletail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/engine"
)

// APIKeyName is the secret keying the agent's API: the same name as Hermes', so switching an agent
// needs no new secret.
const APIKeyName = "API_SERVER_KEY"

type runtime struct{}

func (runtime) APIKeyName() string { return APIKeyName }

// ValidAPIKey: 16+ characters, so an agent's existing key stays valid (new ones are minted longer).
func (runtime) ValidAPIKey(key string) bool { return len(key) >= 16 }

var (
	sourceRe = regexp.MustCompile(`^[a-z_-]{1,24}$`)
	jobName  = regexp.MustCompile(`[^\p{L}\p{N} ._:/()-]`)
	sigRe    = regexp.MustCompile(`^sha256=([0-9a-f]{64})$`)
	// jailRe is a jail report the core passes on: one short printable line
	jailRe = regexp.MustCompile(`^[ -~]{1,200}$`)
)

func isoMillis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Activity is one GET /v1/activity: the runtime reports running jobs itself, so no state is kept.
func (runtime) Activity(ctx context.Context, get func(ctx context.Context, path string) (any, error), _ any, now time.Time) (*engine.Activity, any, error) {
	v, err := get(ctx, "/v1/activity")
	if err != nil {
		return nil, nil, err
	}
	a := ActivityFrom(v, now)
	if a == nil {
		return nil, nil, errors.New("needletail /v1/activity: unexpected body")
	}
	return a, nil, nil
}

// ActivityFrom reads a /v1/activity body (decoded JSON); nil when it is not one.
func ActivityFrom(v any, now time.Time) *engine.Activity {
	if _, ok := v.(map[string]any); !ok {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var w wire.Activity
	if json.Unmarshal(raw, &w) != nil {
		return nil
	}
	a := &engine.Activity{ActiveAgents: w.ActiveTurns, GatewayBusy: w.Busy, RunningJobs: []string{},
		Platforms: map[string]engine.Platform{}, PolledAt: isoMillis(now)}
	if sourceRe.MatchString(w.Source) {
		a.Source = w.Source
	}
	for _, j := range w.RunningJobs {
		r := []rune(jobName.ReplaceAllString(j, ""))
		if len(r) > 60 {
			r = r[:60]
		}
		if len(r) > 0 {
			a.RunningJobs = append(a.RunningJobs, string(r))
		}
	}
	for name, p := range w.Platforms {
		if !sourceRe.MatchString(name) {
			continue
		}
		state := "unknown"
		if sourceRe.MatchString(p.State) {
			state = p.State
		}
		a.Platforms[name] = engine.Platform{State: state, NeedsAttention: p.NeedsAttention}
	}
	if t, err := time.Parse(time.RFC3339Nano, w.LastActive); err == nil {
		a.LastActive = isoMillis(t)
	}
	if t, err := time.Parse(time.RFC3339Nano, w.LastJobAt); err == nil {
		a.LastJobAt = isoMillis(t)
	}
	if jailRe.MatchString(w.Jail) {
		a.Jail = w.Jail
	}
	return a
}

// WakeRequest is POST /v1/runs; the runtime keeps Idempotency-Key for 24 h.
func (runtime) WakeRequest(ctx context.Context, endpoint, input, session, idempotency string) (*http.Request, error) {
	body, _ := json.Marshal(wire.RunRequest{Input: input, SessionID: session})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/runs", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotency)
	return req, nil
}

// HookSignature is the hex of X-Needletail-Signature: sha256=<hex>, "" when absent or malformed.
// Verifying it and dropping a repeated delivery are the core's.
func (runtime) HookSignature(h http.Header) string {
	if m := sigRe.FindStringSubmatch(h.Get(wire.HookSignatureHeader)); m != nil {
		return m[1]
	}
	return ""
}

var hookKinds = map[string]engine.HookKind{
	wire.EventSessionStart: engine.SessionStart, wire.EventSessionEnd: engine.SessionEnd,
	wire.EventToolStart: engine.ToolStart, wire.EventToolDone: engine.ToolDone,
	wire.EventApprovalWaiting: engine.ApprovalWaiting, wire.EventApprovalAnswered: engine.ApprovalAnswered,
}

// ParseHook reads a wire.Hook v1, mapped 1:1 with no fallbacks.
func (runtime) ParseHook(body []byte) (*engine.HookEvent, error) {
	var h wire.Hook
	if err := json.Unmarshal(body, &h); err != nil || h.Event == "" || h.Delivery == "" {
		return nil, errors.New("not a needletail hook delivery")
	}
	if h.V != wire.HookVersion {
		return nil, fmt.Errorf("needletail hook version %d (want %d)", h.V, wire.HookVersion)
	}
	kind, ok := hookKinds[h.Event]
	if !ok {
		return nil, fmt.Errorf("needletail hook event %q", h.Event)
	}
	at, err := time.Parse(time.RFC3339Nano, h.At)
	if err != nil {
		return nil, fmt.Errorf("hook time: %w", err)
	}
	return &engine.HookEvent{Kind: kind, Name: h.Event, Delivery: h.Delivery, At: at, Session: h.Session,
		Tool: h.Tool, ToolInput: h.Input, CallID: h.CallID, ApprovalID: h.ApprovalID, Platform: h.Platform,
		Status: h.Status, Duration: time.Duration(h.DurationMS) * time.Millisecond}, nil
}

func (runtime) LearningKeyName() string { return "NEEDLETAIL_LEARNING_KEY" }
func (runtime) LearningPath(session string) string {
	return "/v1/sessions/" + url.PathEscape(session) + "/learning"
}
