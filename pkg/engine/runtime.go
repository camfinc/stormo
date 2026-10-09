package engine

import (
	"context"
	"net/http"
	"time"
)

// Runtime is how Stormo talks to an agent's running engine through the engine's own API: the
// core's live view, wakes and hook ingest, and `stormo chat`. Requests carry the agent's API key,
// the per-agent secret APIKeyName names.
type Runtime interface {
	APIKeyName() string
	// ValidAPIKey reports whether the engine serves its API with key.
	ValidAPIKey(key string) bool
	// Activity reads what the agent is doing now through get, a GET of a path of its API as JSON
	// (nil, nil when the API answered without a 2xx). state is the runtime's memory of one agent
	// between polls: nil at first, then what the last call returned. A nil Activity means the
	// agent's API is not usable now.
	Activity(ctx context.Context, get func(ctx context.Context, path string) (any, error), state any, now time.Time) (*Activity, any, error)
	// WakeRequest starts a turn: input as the user message, continuing session; idempotency makes a
	// retried wake start one run. The caller adds the key and sends it.
	WakeRequest(ctx context.Context, endpoint, input, session, idempotency string) (*http.Request, error)
	// ChatRequest sends one message in session (`stormo chat`); ChatReply reads the answer.
	ChatRequest(endpoint, session, message string) (*http.Request, error)
	ChatReply(body []byte) (string, error)
	// HookSignature is the hex HMAC-SHA256 of a hook delivery's body (keyed with the agent's core
	// key) from the delivery's headers, "" when absent; ParseHook reads the delivery.
	HookSignature(h http.Header) string
	ParseHook(body []byte) (*HookEvent, error)
}

// Platform is one messaging platform's state as the engine reports it.
type Platform struct {
	State          string `json:"state"`
	NeedsAttention bool   `json:"needsAttention"`
}

// Activity is what the agent is doing right now, from its engine's own API. Counts, states and
// times only: session titles and previews hold client data and never leave the core.
type Activity struct {
	ActiveAgents int  `json:"activeAgents"`
	GatewayBusy  bool `json:"gatewayBusy"`
	// Source of the most recently active session: slack, cron, api_server, telegram, …
	Source string `json:"source,omitempty"`
	// Scheduled jobs running right now (their names: configuration, not client data).
	RunningJobs []string `json:"runningJobs"`
	// When a scheduled job last finished: idle time counts from this or the last session.
	LastJobAt  string              `json:"lastJobAt,omitempty"`
	LastActive string              `json:"lastActive,omitempty"`
	Platforms  map[string]Platform `json:"platforms"`
	PolledAt   string              `json:"polledAt"`
}

// HookKind is what a hook delivery reports, in Stormo's terms.
type HookKind string

const (
	SessionStart     HookKind = "session_start"
	SessionEnd       HookKind = "session_end"
	ToolStart        HookKind = "tool_start"
	ToolDone         HookKind = "tool_done"
	ApprovalWaiting  HookKind = "approval_waiting"
	ApprovalAnswered HookKind = "approval_answered"
	OtherHook        HookKind = "other"
)

// HookEvent is one activity delivery an engine posted to the core.
type HookEvent struct {
	Kind HookKind
	// Name is the engine's own name for the event (kept on the timeline).
	Name     string
	Delivery string // unique per delivery: a retry is recognised by it
	At       time.Time
	Session  string
	Tool     string
	// ToolInput is the tool call's arguments (only file paths and command starts are kept).
	ToolInput map[string]any
	// CallID pairs a tool's start and end; ApprovalID an approval's request and answer.
	CallID, ApprovalID string
	// Platform a session runs on; Status and Duration of a finished tool call.
	Platform string
	Status   string
	Duration time.Duration
}
