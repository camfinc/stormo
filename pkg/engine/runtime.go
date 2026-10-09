package engine

import (
	"context"
	"errors"
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
	// Conversations is the agent's chat surface (`stormo chat`, `stormo conversations`, the app's
	// Chat): its sessions, their transcripts, one turn, and clearing one. call sends one request to
	// the agent's API with its key.
	Conversations(call APICall) Conversations
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

// APICall sends one request to an agent's engine API with its key: body is marshalled as JSON when
// not nil. It returns the HTTP status and the response body (any status: the engine reads it).
type APICall func(ctx context.Context, method, path string, body any) (status int, resp []byte, err error)

// Conversations reads and drives an agent's sessions through its engine API. Titles, previews and
// messages are client data: they go to the caller (the owner's CLI and app), never to the core.
type Conversations interface {
	List(ctx context.Context, limit int) ([]Conversation, error)
	// Transcript is a conversation as the agent holds it now: after the engine compacted it, its
	// summary replaces the older turns (Compacted, with Tip the session that carries it on).
	Transcript(ctx context.Context, id string, limit int) (*Transcript, error)
	// New starts an empty conversation, its id generated when id is "".
	New(ctx context.Context, id, title string) (string, error)
	// Send runs one turn in conversation id, created when it does not exist yet. It returns the
	// reply and the session that holds the conversation now (a compaction can move it).
	Send(ctx context.Context, id, message string) (reply, tip string, err error)
	// Delete removes a conversation (id: any of its sessions) and the sessions its compactions
	// continued in; the number of sessions removed, 0 when it did not exist. Hermes: the chain is
	// found among the 200 most recently active sessions.
	Delete(ctx context.Context, id string) (int, error)
}

// Conversation is one session as the engine lists it.
type Conversation struct {
	ID         string `json:"id"`
	Title      string `json:"title,omitempty"`
	Source     string `json:"source,omitempty"`
	Preview    string `json:"preview,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	LastActive string `json:"lastActive,omitempty"`
	Messages   int    `json:"messages"`
	Ended      bool   `json:"ended"`
	// Compacted: the engine folded older turns into a summary (ID stays the conversation's first
	// session; the counts and times are the live part's).
	Compacted bool `json:"compacted"`
}

// Message kinds: what a client shows differently.
const (
	MessageText = "text"
	MessageTool = "tool"
)

// Message is one visible transcript row.
type Message struct {
	ID      string   `json:"id,omitempty"`
	Role    string   `json:"role"`
	Kind    string   `json:"kind"`
	Content string   `json:"content"`
	At      string   `json:"at,omitempty"`
	Tools   []string `json:"tools,omitempty"` // the tools an assistant row called
	Tool    string   `json:"tool,omitempty"`  // the tool a tool row answers for
}

// Transcript is a conversation's visible messages, oldest first.
type Transcript struct {
	ID        string    `json:"id"`
	Tip       string    `json:"tip"`
	Compacted bool      `json:"compacted"`
	Messages  []Message `json:"messages"`
}

// ErrNotFound is a conversation the engine does not hold.
var ErrNotFound = errors.New("conversation not found")

// ErrBusy is an engine refusing a turn because it already runs as many as it allows.
var ErrBusy = errors.New("the agent is busy with other turns; try again shortly")
