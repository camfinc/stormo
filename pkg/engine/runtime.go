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
	// Jail is how the engine confines the commands the agent executes, when it reports it (a
	// short line, such as "landlock ABI 6" or "off: <why>"); empty when it does not.
	Jail string `json:"jail,omitempty"`
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

// ConversationCommands is a Conversations that takes a person's chat commands for a channel the
// core runs (WhatsApp's /new and /trust; docs/whatsapp.md): optional. by names the person
// ("whatsapp:+…"); a conversation a turn runs in answers ErrBusy, one never started ErrNotFound.
type ConversationCommands interface {
	// Restart goes on with conversation id in a fresh session (the earlier one archived).
	Restart(ctx context.Context, id, by string) error
	// Trust endorses conversation id as its owner's trust does; marked is whether it was.
	Trust(ctx context.Context, id, by string) (marked bool, err error)
}

// Exchanger is a Conversations whose turns carry files in and a voice note out (needletail's
// API): a person's voice note to transcribe, the agent's spoken reply to post as one.
type Exchanger interface {
	Exchange(ctx context.Context, id string, in Input) (*Reply, error)
}

// Input is a turn's input: its text, the files it carries, and how a voice reply comes back
// (AudioFormat mp3, the default, or opus: a voice note's Ogg Opus).
type Input struct {
	Text        string
	Files       []File
	AudioFormat string
}

// File is a file an input carries; Marked when its sender may not instruct the agent.
type File struct {
	Name, MIME string
	Data       []byte
	Marked     bool
}

// Reply is a turn's answer: its text, the session that holds the conversation now, and the voice
// note the agent answered with (nil: none). WithText: the text goes with the note.
type Reply struct {
	Text, Tip string
	Audio     *Audio
}

// Audio is a reply spoken.
type Audio struct {
	MIME, Name string
	Data       []byte
	WithText   bool
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

// SourceFollowUp is a Message's Source for a user row no one wrote: the input of a turn the
// agent's own follow-up started (a reminder it set itself), which a client labels as such
// rather than as the person's.
const SourceFollowUp = "follow_up"

// Message is one visible transcript row.
type Message struct {
	ID      string   `json:"id,omitempty"`
	Role    string   `json:"role"`
	Kind    string   `json:"kind"`
	Content string   `json:"content"`
	At      string   `json:"at,omitempty"`
	Tools   []string `json:"tools,omitempty"` // the tools an assistant row called
	Tool    string   `json:"tool,omitempty"`  // the tool a tool row answers for
	// Source is who wrote a user row when it was not the person (SourceFollowUp); empty
	// otherwise.
	Source string `json:"source,omitempty"`
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

// LearningRuntime is an optional owner-review API. Its dedicated credential is
// resolved and sent only by the core; it is never an ordinary conversation key.
type LearningRuntime interface {
	LearningKeyName() string
	LearningPath(session string) string
}

// Channel turns (docs/core.md §5, "Returns"): turns the core runs for conversations of its own
// channels (WhatsApp), admitted once per execution and signed with the agent's core key, so a turn
// is never run twice and what it did is attributed to the interaction that started it. An optional
// Runtime capability: an engine without it keeps Conversations.

// RawCall sends one request to an agent's engine API with its key and the extra headers given.
type RawCall func(ctx context.Context, method, path string, body []byte, header http.Header) (status int, resp []byte, err error)

// ChannelTurner is a Runtime that runs channel turns.
type ChannelTurner interface {
	// ChannelTurns is the client, its requests signed with the agent's core key.
	ChannelTurns(call RawCall, coreKey string) ChannelTurns
}

// ChannelTurns drives an agent's channel turns.
type ChannelTurns interface {
	Capabilities(ctx context.Context) (ChannelCaps, error)
	// Start admits the turn (a repeat of the same execution answers its status). ErrChannelBusy:
	// the conversation runs another turn, nothing admitted; ErrChannelRefused: the runtime answered
	// that it will not run it, nothing admitted. Any other error may have admitted it: the caller
	// asks again with the same execution and the same request.
	Start(ctx context.Context, t ChannelTurn) (ChannelTurnStatus, error)
	// Status is the turn's, waiting up to wait for it to end.
	Status(ctx context.Context, exec string, wait time.Duration) (ChannelTurnStatus, error)
	// Take tells the engine the core kept the result: it forgets it, never the execution.
	Take(ctx context.Context, exec string) error
}

// ChannelCaps is what the engine offers: channel turns, and whether the channel's conversations run
// only so (enforcing).
type ChannelCaps struct{ Turns, Enforcing bool }

// ChannelTurn is a turn to run: Kind "inbound" (a person's message) or "return" (the outcome of a
// request sent from it).
type ChannelTurn struct {
	Execution, Kind, Session, Title string
	// Token is the execution's secret, the core's: the runtime keeps it in memory only and sends it
	// with every core call the turn makes, which is how the core knows the call is that turn's.
	Token string
	In    Input
}

// ChannelTurnStatus is a channel turn's state (running, ok, failed, interrupted) and, once it
// ended, its result. Acted: the tool calls it made (one that acted is never run again). Marked:
// its reply carries content the agent must not obey. Taken: the result was forgotten.
type ChannelTurnStatus struct {
	Execution, State, Tip, Text, Error string
	Audio                              *Audio
	Acted                              int64
	Marked, Taken                      bool
}

// Terminal is whether the turn ended.
func (s ChannelTurnStatus) Terminal() bool {
	return s.State == "ok" || s.State == "failed" || s.State == "interrupted"
}

// Channel turn errors.
var (
	ErrChannelBusy    = errors.New("the conversation runs another turn: nothing admitted, try again")
	ErrChannelRefused = errors.New("the runtime refused the channel turn: nothing admitted")
	ErrChannelUnknown = errors.New("no such channel turn")
)
