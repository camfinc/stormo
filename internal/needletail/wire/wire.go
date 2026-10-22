// Package wire is the needletail engine's wire and file formats: the JSON and YAML types the
// Stormo adapter (pkg/engine/needletail) and the in-container runtime both read and write, so each
// has one definition. Every type here is a versioned contract (docs/needletail.md §8): a change
// needs a new version and a migration.
package wire

import "regexp"

// Capability is the Luau capability grammar (docs/needletail.md §5.4): what a tool's header
// declares and what engine/needletail/config.yaml grants, read by the adapter and the runtime.
//
//	fs.read:home  fs.read:shared  fs.write:home[/<glob>]  fs.write:shared
//	http:<host>   http:*.<suffix>  core:<tool>  core:<prefix>*
//	message:<target>  message:<target prefix>:*  message:*   (send_message's targets: slack,
//	                  slack:<channel>, slack:<channel>:<thread_ts>, slack:<user>)
var Capability = regexp.MustCompile(`^(fs\.read:(home|shared)|fs\.write:(home(/[^\s]+)?|shared)|http:(\*\.)?[a-z0-9-]+(\.[a-z0-9-]+)+|core:[a-z][a-z0-9_]*\*?|message:(\*|slack(:\*|:[A-Z0-9]+(:\*|:[0-9]+\.[0-9]+)?)?))$`)

// Contract versions.
const (
	// HookVersion is Hook's "v".
	HookVersion = 1
	// SessionVersion is a session file header's "v".
	SessionVersion = 1
	// ConfigVersion is needletail.yaml's "version".
	ConfigVersion = 1
)

// HookSignatureHeader carries a hook's signature: "sha256=<hex HMAC-SHA256(body, core key)>".
const HookSignatureHeader = "X-Needletail-Signature"

// Hook events (Hook.Event).
const (
	EventSessionStart     = "session_start"
	EventSessionEnd       = "session_end"
	EventToolStart        = "tool_start"
	EventToolDone         = "tool_done"
	EventApprovalWaiting  = "approval_waiting"
	EventApprovalAnswered = "approval_answered"
)

// Hook is one activity delivery to the core (POST {ingest_url}/needletail), v1.
type Hook struct {
	V          int            `json:"v"`
	Event      string         `json:"event"`
	Delivery   string         `json:"delivery"`
	At         string         `json:"at"` // RFC 3339 with nanoseconds
	Session    string         `json:"session"`
	Tool       string         `json:"tool"`
	Input      map[string]any `json:"input"`
	CallID     string         `json:"call_id"`
	ApprovalID string         `json:"approval_id"`
	Platform   string         `json:"platform"`
	Status     string         `json:"status"`
	DurationMS int64          `json:"duration_ms"`
}

// Platform is one messaging platform's state in Activity.
type Platform struct {
	State          string `json:"state"`
	NeedsAttention bool   `json:"needs_attention"`
}

// Activity is GET /v1/activity: counts, states, ids and times only (no titles or previews).
type Activity struct {
	ActiveTurns int                 `json:"active_turns"`
	Busy        bool                `json:"busy"`
	Source      string              `json:"source,omitempty"`
	RunningJobs []string            `json:"running_jobs"`
	LastJobAt   string              `json:"last_job_at,omitempty"`
	LastActive  string              `json:"last_active,omitempty"`
	Platforms   map[string]Platform `json:"platforms"`
}

// RunRequest is POST /v1/runs (with an Idempotency-Key header); the API answers 202 with RunAccepted.
type RunRequest struct {
	Input     string `json:"input"`
	SessionID string `json:"session_id"`
}

// RunAccepted is POST /v1/runs' 202 body.
type RunAccepted struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
}

// Session is a session file's header line (sessions/<id>.jsonl, v1). A compaction ends a session
// (EndReason "compression") and continues it in a new one whose Parent is the old id.
type Session struct {
	V         int    `json:"v"`
	ID        string `json:"id"`
	Parent    string `json:"parent,omitempty"`
	EndReason string `json:"end_reason,omitempty"`
	Created   string `json:"created"`
	Source    string `json:"source,omitempty"`
	Title     string `json:"title,omitempty"`
}

// EndCompression is Session.EndReason for a session a compaction continued elsewhere.
const EndCompression = "compression"

// SessionSummary is one conversation in GET /v1/sessions: listed as its live tip (ID), with Root
// naming the conversation's first session when compactions moved it.
type SessionSummary struct {
	Session
	Root         string `json:"root,omitempty"`
	Preview      string `json:"preview,omitempty"`
	LastActive   string `json:"last_active,omitempty"`
	MessageCount int    `json:"message_count"`
	Ended        bool   `json:"ended"`
}

// SessionList is GET /v1/sessions.
type SessionList struct {
	Data []SessionSummary `json:"data"`
}

// NewSession is POST /v1/sessions (409 when ID exists).
type NewSession struct {
	ID     string `json:"id,omitempty"`
	Title  string `json:"title,omitempty"`
	Source string `json:"source,omitempty"`
}

// ToolCall is an assistant message's tool call; ID is the model's own call id, kept verbatim.
type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// Message is a session file's message line, and a transcript row.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	// ToolName is the tool a tool message answers for (transcripts only; files pair by id).
	ToolName string `json:"tool_name,omitempty"`
	At       string `json:"at,omitempty"`
}

// Transcript is GET /v1/sessions/{id}/messages: the conversation's messages, oldest first,
// following its compactions; SessionID is its live tip.
type Transcript struct {
	SessionID string    `json:"session_id"`
	Data      []Message `json:"data"`
}

// ChatRequest is POST /v1/sessions/{id}/chat.
type ChatRequest struct {
	Message string `json:"message"`
}

// ChatResponse is the chat reply; SessionID is the tip that holds the conversation now.
type ChatResponse struct {
	SessionID string `json:"session_id"`
	Message   struct {
		Content string `json:"content"`
	} `json:"message"`
}

// Deleted is DELETE /v1/sessions/{id}: the sessions of the lineage removed.
type Deleted struct {
	Deleted int `json:"deleted"`
}

// Error is an API error body.
type Error struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// JobSchedule is a job's timing: kind "interval" (Minutes) or "cron" (Expr), as Hermes writes it.
type JobSchedule struct {
	Kind    string `json:"kind"`
	Minutes int    `json:"minutes,omitempty"`
	Expr    string `json:"expr,omitempty"`
	Display string `json:"display,omitempty"`
}

// JobRepeat counts a job's runs; Times nil is forever.
type JobRepeat struct {
	Times     *int `json:"times"`
	Completed int  `json:"completed"`
}

// Job is one record of cron/jobs.json ({"jobs": [...]}), the shape Hermes writes, so nap,
// rehydrate and the dream treat both engines' files alike. Running is needletail's own state key.
type Job struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	Prompt          string      `json:"prompt"`
	Skills          []string    `json:"skills"`
	Skill           *string     `json:"skill"`
	Model           *string     `json:"model"`
	Provider        *string     `json:"provider"`
	Script          *string     `json:"script"`
	NoAgent         bool        `json:"no_agent"`
	MonitorScript   *string     `json:"monitor_script"`
	Schedule        JobSchedule `json:"schedule"`
	ScheduleDisplay string      `json:"schedule_display"`
	Repeat          JobRepeat   `json:"repeat"`
	Enabled         bool        `json:"enabled"`
	State           string      `json:"state"`
	PausedReason    *string     `json:"paused_reason"`
	Deliver         *string     `json:"deliver"`
	EnabledToolsets []string    `json:"enabled_toolsets"`
	FailureDeliver  *string     `json:"failure_deliver"`

	NextRunAt         *string `json:"next_run_at"`
	LastRunAt         *string `json:"last_run_at"`
	LastStatus        *string `json:"last_status"`
	LastError         *string `json:"last_error"`
	LastDeliveryError *string `json:"last_delivery_error"`
	FailureStreak     int     `json:"failure_streak,omitempty"`
	Running           bool    `json:"running,omitempty"`
}

// Job statuses (Job.LastStatus): silent and no_change are successful runs with nothing to say.
const (
	StatusOK       = "ok"
	StatusSilent   = "silent"
	StatusNoChange = "no_change"
	StatusError    = "error"
)

// Config is $AGENT_HOME/needletail.yaml, rendered by the adapter and read once at boot.
type Config struct {
	Version   int          `json:"version" yaml:"version"`
	Agent     string       `json:"agent" yaml:"agent"`
	Model     ModelConfig  `json:"model" yaml:"model"`
	Reasoning string       `json:"reasoning,omitempty" yaml:"reasoning,omitempty"`
	Limits    Limits       `json:"limits" yaml:"limits"`
	Core      *CoreConfig  `json:"core,omitempty" yaml:"core,omitempty"`
	Channels  []string     `json:"channels,omitempty" yaml:"channels,omitempty"`
	Memory    MemoryConfig `json:"memory" yaml:"memory"`
	Skills    SkillsConfig `json:"skills,omitempty" yaml:"skills,omitempty"`
	Luau      LuauConfig   `json:"luau" yaml:"luau"`
	// Timezone is the IANA zone the agent's wall clock is on (schedules' cron expressions, the
	// system prompt's time); "": the container's $TZ, else UTC.
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	// Slack is the agent's own Slack settings (engine/needletail/config.yaml's slack:); the
	// channel's settings stay in its SLACK_* env.
	Slack *SlackConfig `json:"slack,omitempty" yaml:"slack,omitempty"`
	// Schedules is what the runtime is told about the agent's jobs.
	Schedules SchedulesConfig `json:"schedules,omitempty" yaml:"schedules,omitempty"`
}

// SlackConfig is the agent's grants for Slack.
type SlackConfig struct {
	// Broadcast lets a send_message call that asks for it (broadcast: true) ping a crowd:
	// @channel, @here, @everyone and user groups. Without it every such token is escaped.
	Broadcast bool `json:"broadcast,omitempty" yaml:"broadcast,omitempty"`
}

// SchedulesConfig is what the runtime is told about the agent's scheduled jobs.
type SchedulesConfig struct {
	// Turns is how many scheduled runs may hold a turn at once, in their own lane beside the
	// chat turns (0: the runtime's default, 1).
	Turns int `json:"turns,omitempty" yaml:"turns,omitempty"`
	// Repo are the ids of the jobs agent.yaml defines: their delivery is trusted as the repo
	// wrote it, and they never count toward the jobs the agent may make.
	Repo []string `json:"repo,omitempty" yaml:"repo,omitempty"`
}

// SkillsConfig is what the runtime is told about the home's skills. Generated are the skill dirs
// (relative to skills/) the build generates from the instance, such as its knowledge and
// shared-docs skills: the agent may use them but skill_manage never changes them (the dream never
// proposes them, so a change would only be lost at the next build).
type SkillsConfig struct {
	Generated []string `json:"generated,omitempty" yaml:"generated,omitempty"`
}

// Endpoint is an OpenAI chat-completions endpoint; KeyEnv names the env var holding its key.
type Endpoint struct {
	BaseURL string `json:"base_url" yaml:"base_url"`
	KeyEnv  string `json:"key_env,omitempty" yaml:"key_env,omitempty"`
	Model   string `json:"model" yaml:"model"`
}

// ModelConfig is the primary endpoint and the ones tried, in order, when it fails.
type ModelConfig struct {
	Primary   Endpoint   `json:"primary" yaml:"primary"`
	Fallbacks []Endpoint `json:"fallbacks,omitempty" yaml:"fallbacks,omitempty"`
}

// Limits bound a turn (0: the runtime's default). Timeouts are seconds.
type Limits struct {
	Turns          int `json:"turns,omitempty" yaml:"turns,omitempty"`
	CommandTimeout int `json:"command_timeout,omitempty" yaml:"command_timeout,omitempty"`
	ScriptTimeout  int `json:"script_timeout,omitempty" yaml:"script_timeout,omitempty"`
	// LuauTimeout bounds a Luau tool the model calls (a schedule's .luau script has
	// ScriptTimeout); a tool's header may only lower it.
	LuauTimeout int `json:"luau_timeout,omitempty" yaml:"luau_timeout,omitempty"`
}

// CoreConfig is the swarm core, set only when the agent runs on it.
type CoreConfig struct {
	MCPURL    string `json:"mcp_url" yaml:"mcp_url"`
	IngestURL string `json:"ingest_url" yaml:"ingest_url"`
	KeyEnv    string `json:"key_env" yaml:"key_env"`
}

// MemoryConfig is the hot-memory budgets, in UTF-16 units of each file.
type MemoryConfig struct {
	MemoryCharLimit int `json:"memory_char_limit" yaml:"memory_char_limit"`
	UserCharLimit   int `json:"user_char_limit" yaml:"user_char_limit"`
}

// LuauConfig is what Luau tools may be granted (docs/needletail.md §5.4): a tool's effective
// capabilities are the ones it declares that appear in Grants.
type LuauConfig struct {
	Grants      []string     `json:"grants" yaml:"grants"`
	HTTPSecrets []HTTPSecret `json:"http_secrets,omitempty" yaml:"http_secrets,omitempty"`
}

// HTTPSecret injects the value of env Env into header Header (Template's "{value}" replaced) on
// requests to Host. Luau never sees the value.
type HTTPSecret struct {
	Host     string `json:"host" yaml:"host"`
	Env      string `json:"env" yaml:"env"`
	Header   string `json:"header" yaml:"header"`
	Template string `json:"template,omitempty" yaml:"template,omitempty"`
}

// Users of the runtime's container (docs/needletail.md §7). The runtime runs as RuntimeUID (the
// shared space's owner, shared.PosixUID, which the adapter's Layout and the EFS access points
// use) and owns its own state; everything the agent executes (the terminal, scheduled scripts
// and monitors) runs as ExecUID with the runtime's group, so its files stay out of their reach.
const (
	RuntimeUID = 10000
	RuntimeGID = 10000
	ExecUID    = 10001
)
