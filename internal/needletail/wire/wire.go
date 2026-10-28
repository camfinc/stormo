// Package wire is the needletail engine's wire and file formats: the JSON and YAML types the
// Stormo adapter (pkg/engine/needletail) and the in-container runtime both read and write, so each
// has one definition. Every type here is a versioned contract (docs/needletail.md §8): a change
// needs a new version and a migration.
package wire

import (
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Capability is the Luau capability grammar (docs/needletail.md §5.4): what a tool's header
// declares and what engine/needletail/config.yaml grants, read by the adapter and the runtime.
//
//	fs.read:home  fs.read:shared  fs.write:home[/<glob>]  fs.write:shared
//	http:<host>   http:*.<suffix>  core:<tool>  core:<prefix>*
//	message:<target>  message:<target prefix>:*  message:*   (send_message's targets: slack,
//	                  slack:<channel>, slack:<channel>:<thread_ts>, slack:<user>)
//	memory:read  memory:write   (the memory archive: search_memory; save, update, delete_memory)
//	kv:read  kv:write           (the agent's key-value store: kv_get; kv_set, kv_delete)
//	followup:read  followup:write  (one-time follow-ups: nt.followup.list; follow_up, nt.followup.cancel)
var Capability = regexp.MustCompile(`^(fs\.read:(home|shared)|fs\.write:(home(/[^\s]+)?|shared)|http:(\*\.)?[a-z0-9-]+(\.[a-z0-9-]+)+|core:[a-z][a-z0-9_]*\*?|message:(\*|slack(:\*|:[A-Z0-9]+(:\*|:[0-9]+\.[0-9]+)?)?)|memory:(read|write)|kv:(read|write)|followup:(read|write))$`)

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
	// Jail is how the agent's commands are confined: "landlock ABI <n>", or "off: <why>" (a
	// kernel without Landlock, the uid split off). Absent from runtimes before the jail.
	Jail string `json:"jail,omitempty"`
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
	// Untrusted: the conversation has taken in content the agent must not obey (an unverified
	// author's words, overheard input, unverified thread context, a follow-up text made from
	// such input or by a handler). Set on the session that took it in and every session a
	// compaction continues it in, never cleared: such a conversation defers no work
	// (docs/needletail.md §7). Absent in files from before it: not marked. A cache of the rows:
	// true when a row after Endorsed is marked (or marked data was read into the conversation);
	// cleared only by the owner's endorsement (Endorsed).
	Untrusted bool `json:"untrusted,omitempty"`
	// Taint is who and what brought the marked data in (at most MaxTaint, the first kept): no
	// author named here may approve the conversation's actions. Added with approvals.
	Taint []Taint `json:"taint,omitempty"`
	// Endorsed is how many of the session file's rows the owner endorsed ("trust thread"): their
	// marks no longer taint the conversation; EndorsedBy and EndorsedAt say who and when. Added
	// with approvals; absent: none.
	Endorsed   int    `json:"endorsed,omitempty"`
	EndorsedBy string `json:"endorsed_by,omitempty"`
	EndorsedAt string `json:"endorsed_at,omitempty"`
}

// Taint is one source of a conversation's marked data (Session.Taint): the people (Slack user or
// bot ids) whose words it is, whether they passed allowed_users (a relay bot did, and still
// marks), when, and where (a Slack channel and thread) or what (a marked memory record, a key, a
// conversation read).
type Taint struct {
	Why      string   `json:"why"`
	Users    []string `json:"users,omitempty"`
	Verified bool     `json:"verified,omitempty"`
	At       string   `json:"at,omitempty"`
	Channel  string   `json:"channel,omitempty"`
	Thread   string   `json:"thread,omitempty"`
}

// MaxTaint is the most Taint entries a session header keeps.
const MaxTaint = 8

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

// NewSession is POST /v1/sessions (409 when ID exists; 403 for an id the runtime keeps for its
// own conversations, api.Reserved).
type NewSession struct {
	ID    string `json:"id,omitempty"`
	Title string `json:"title,omitempty"`
	// Source is ignored: the API's sessions are always api_server (kept for the adapter, which
	// sends it).
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
	// Kind marks a user row no one wrote (KindFollowUp: a follow-up's turn input); empty for
	// everything else.
	Kind string `json:"kind,omitempty"`
	// Untrusted marks a row that carries content the agent must not obey (a turn's input from
	// an unverified or overheard author, or with unverified thread context; a follow-up text
	// made from such input or by a handler); storing one marks its conversation (Session.Untrusted).
	Untrusted bool `json:"untrusted,omitempty"`
}

// KindFollowUp is a Message's Kind for a follow-up's turn input (docs/needletail.md §4.12), and
// for the assistant row a delivered follow-up text leaves.
const KindFollowUp = "follow_up"

// KindApproval is a Message's Kind for the runtime's note of an approval's outcome (a user row no
// one wrote, docs/needletail.md §7).
const KindApproval = "approval"

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
	BackgroundLearning bool         `json:"background_learning,omitempty" yaml:"background_learning,omitempty"`
	SkillMaintenance   bool         `json:"skill_maintenance,omitempty" yaml:"skill_maintenance,omitempty"`
	Reflection         bool         `json:"reflection,omitempty" yaml:"reflection,omitempty"`
	Version            int          `json:"version" yaml:"version"`
	Agent              string       `json:"agent" yaml:"agent"`
	Model              ModelConfig  `json:"model" yaml:"model"`
	Reasoning          string       `json:"reasoning,omitempty" yaml:"reasoning,omitempty"`
	Limits             Limits       `json:"limits" yaml:"limits"`
	Core               *CoreConfig  `json:"core,omitempty" yaml:"core,omitempty"`
	Channels           []string     `json:"channels,omitempty" yaml:"channels,omitempty"`
	Memory             MemoryConfig `json:"memory" yaml:"memory"`
	Skills             SkillsConfig `json:"skills,omitempty" yaml:"skills,omitempty"`
	Luau               LuauConfig   `json:"luau" yaml:"luau"`
	// Timezone is the IANA zone the agent's wall clock is on (schedules' cron expressions, the
	// system prompt's time); "": the container's $TZ, else UTC.
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	// Slack is the agent's own Slack settings (engine/needletail/config.yaml's slack:); the
	// channel's settings stay in its SLACK_* env.
	Slack *SlackConfig `json:"slack,omitempty" yaml:"slack,omitempty"`
	// Schedules is what the runtime is told about the agent's jobs.
	Schedules SchedulesConfig `json:"schedules,omitempty" yaml:"schedules,omitempty"`
	// StateRoot is the manifest's state dir (the instance's names.state_dir, the engine layout's
	// StateRoot), relative to the home: part of the agent's workspace, which the uid split keeps
	// reachable by the runtime.
	StateRoot string `json:"state_root,omitempty" yaml:"state_root,omitempty"`
	// Scrub is the instance's own scrub rules (stormo.yaml scrub:), so what the runtime exports for
	// the dream is checked by the scrubber the dream uses. Absent when the instance has none.
	Scrub *ScrubConfig `json:"scrub,omitempty" yaml:"scrub,omitempty"`
	// Approvals is engine/needletail/config.yaml's approvals: who decides, how long a request
	// waits, the caps and the risky commands (docs/needletail.md §7). Absent: the defaults (the
	// first allowed user decides). An image older than it refuses a needletail.yaml that has it.
	Approvals *ApprovalsConfig `json:"approvals,omitempty" yaml:"approvals,omitempty"`
	// Files is engine/needletail/config.yaml's files: how long people's uploads are kept and
	// whether images reach the model. Absent: 14 days, vision auto. An image older than it
	// refuses a needletail.yaml that has it.
	Files *FilesConfig `json:"files,omitempty" yaml:"files,omitempty"`
	// STT is engine/needletail/config.yaml's stt: voice notes transcribed before the turn.
	// Absent or not enabled: none are (the note is kept as a file). An image older than it
	// refuses a needletail.yaml that has it.
	STT *STTConfig `json:"stt,omitempty" yaml:"stt,omitempty"`
	// Web is engine/needletail/config.yaml's web: the search provider web_search asks. Absent:
	// no web_search (web_extract needs no key). An image older than it refuses a needletail.yaml
	// that has it.
	Web *WebConfig `json:"web,omitempty" yaml:"web,omitempty"`
}

// WebConfig is the agent's reach into the web.
type WebConfig struct {
	Search *WebSearchConfig `json:"search,omitempty" yaml:"search,omitempty"`
}

// WebSearchConfig is web_search's provider: brave (its key BRAVE_SEARCH_API_KEY) or tavily
// (TAVILY_API_KEY), and how many results a search gives (0: 5; at most 20).
type WebSearchConfig struct {
	Provider   string `json:"provider" yaml:"provider"`
	MaxResults int    `json:"max_results,omitempty" yaml:"max_results,omitempty"`
}

// Search providers and their keys' env names.
const (
	SearchBrave  = "brave"
	SearchTavily = "tavily"
)

// SearchKeyEnv is the env var holding provider's key ("" for a provider this runtime has not).
func SearchKeyEnv(provider string) string {
	switch provider {
	case SearchBrave:
		return "BRAVE_SEARCH_API_KEY"
	case SearchTavily:
		return "TAVILY_API_KEY"
	}
	return ""
}

// MaxSearchResults is the most results web.search.max_results may ask for.
const MaxSearchResults = 20

// FilesConfig is what the runtime does with the files people send.
type FilesConfig struct {
	// RetentionDays is how long an upload is kept in files/<conversation>/ (0: 14).
	RetentionDays int `json:"retention_days,omitempty" yaml:"retention_days,omitempty"`
	// Vision is whether images reach the model: auto ("" too: sent, unless the model refuses
	// them), on or off.
	Vision Vision `json:"vision,omitempty" yaml:"vision,omitempty"`
}

// Vision is a vision setting: auto, on or off ("" is auto). YAML's true and false read as on and
// off.
type Vision string

// UnmarshalYAML reads auto, on, off, true or false.
func (v *Vision) UnmarshalYAML(n *yaml.Node) error {
	switch strings.ToLower(strings.TrimSpace(n.Value)) {
	case "", "auto":
		*v = "auto"
	case "on", "true":
		*v = "on"
	case "off", "false":
		*v = "off"
	default:
		return fmt.Errorf("vision %q is auto, on or off", n.Value)
	}
	return nil
}

// STTConfig is the speech-to-text of voice notes.
type STTConfig struct {
	Enabled bool `json:"enabled" yaml:"enabled"`
	// Provider is elevenlabs (the only one; "" is it). Its key is the env ELEVENLABS_API_KEY.
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
	// Language is the notes' language, an ISO 639-1 or 639-3 code ("": detected).
	Language string `json:"language,omitempty" yaml:"language,omitempty"`
	// Model is the provider's model id ("": the runtime's default).
	Model string `json:"model,omitempty" yaml:"model,omitempty"`
}

// ApprovalsConfig is the agent's approvals settings.
type ApprovalsConfig struct {
	// Approvers are the Slack user ids who decide (within allowed_users); empty: the first user
	// allowed_users names.
	Approvers []string `json:"approvers,omitempty" yaml:"approvers,omitempty"`
	// Timeout is how long a request waits for a decision, in seconds (0: 1800); past it, denied.
	Timeout int `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	// DeliveryExpiryDays is how long a tainted scheduled run's held report waits (0: 3 days).
	DeliveryExpiryDays int `json:"delivery_expiry_days,omitempty" yaml:"delivery_expiry_days,omitempty"`
	// PerConversation is the most pending requests of one conversation (0: 3); PerDay of the
	// agent in 24 hours (0: 20). Past them a request is refused.
	PerConversation int `json:"per_conversation,omitempty" yaml:"per_conversation,omitempty"`
	PerDay          int `json:"per_day,omitempty" yaml:"per_day,omitempty"`
	// Commands are risky-command patterns added to the defaults: a terminal command one matches
	// needs approval in any conversation and is refused in a scheduled run.
	Commands []CommandRule `json:"commands,omitempty" yaml:"commands,omitempty"`
}

// CommandRule is one risky-command pattern (a Go regular expression, matched case insensitively)
// and what it means.
type CommandRule struct {
	Name    string `json:"name,omitempty" yaml:"name,omitempty"`
	Pattern string `json:"pattern" yaml:"pattern"`
}

// ScrubConfig is stormo.yaml's scrub: (instance.Instance ScrubTokenPrefixes and ScrubRules).
type ScrubConfig struct {
	TokenPrefixes []string    `json:"token_prefixes,omitempty" yaml:"token_prefixes,omitempty"`
	Rules         []ScrubRule `json:"rules,omitempty" yaml:"rules,omitempty"`
}

// ScrubRule is one of the instance's scrub rules: a finding of Kind where Pattern matches and
// every Require pattern matches the same text.
type ScrubRule struct {
	Kind    string   `json:"kind" yaml:"kind"`
	Pattern string   `json:"pattern" yaml:"pattern"`
	Require []string `json:"require,omitempty" yaml:"require,omitempty"`
}

// SlackConfig is the agent's grants for Slack.
type SlackConfig struct {
	// Broadcast lets a send_message call that asks for it (broadcast: true) ping a crowd:
	// @channel, @here, @everyone and user groups. Without it every such token is escaped.
	Broadcast bool `json:"broadcast,omitempty" yaml:"broadcast,omitempty"`
	// Listen are the channels the agent listens to: channel ids, or "*" for every public or
	// private channel the bot is in. Every message there that the agent would not answer (no
	// mention, not a thread it is in) is ambient: it reaches the on_message handlers only, and
	// a turn follows only when one passes it on.
	Listen []string `json:"listen,omitempty" yaml:"listen,omitempty"`
	// RelayBots are bots (bot ids B…, or their user ids) allowed_users lets talk to the agent whose
	// messages still mark the conversation: they relay others' words (a form, an email bridge), so
	// passing allowed_users says who posted, not who wrote.
	RelayBots []string `json:"relay_bots,omitempty" yaml:"relay_bots,omitempty"`
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
	// BackgroundLifetime bounds a background process (terminal background: true): it is killed
	// this long after it started (0: the runtime's 2 h). An image older than the key refuses a
	// needletail.yaml that has it.
	BackgroundLifetime int `json:"background_lifetime,omitempty" yaml:"background_lifetime,omitempty"`
	// ChatTurns is how many chat turns (the API's and the Slack gateway's) may run at once
	// (0: the runtime's 2; 1 to 16). The core's gateway bounds model calls across the fleet
	// besides (stormo.yaml core.llm_concurrency). An image older than the key refuses a
	// needletail.yaml that has it.
	ChatTurns int `json:"chat_turns,omitempty" yaml:"chat_turns,omitempty"`
}

// CoreConfig is the swarm core, set only when the agent runs on it.
type CoreConfig struct {
	MCPURL    string `json:"mcp_url" yaml:"mcp_url"`
	IngestURL string `json:"ingest_url" yaml:"ingest_url"`
	KeyEnv    string `json:"key_env" yaml:"key_env"`
}

// MemoryConfig is the hot-memory budgets, in UTF-16 units of each file, and the per-turn recall
// from the memory archive.
type MemoryConfig struct {
	MemoryCharLimit int `json:"memory_char_limit" yaml:"memory_char_limit"`
	UserCharLimit   int `json:"user_char_limit" yaml:"user_char_limit"`
	// Recall is engine/needletail/config.yaml's memory.recall; omitted when unset (the runtime's
	// defaults: on for user turns, off for scheduled runs, 5 records, 1,200 units).
	Recall *RecallConfig `json:"recall,omitempty" yaml:"recall,omitempty"`
}

// RecallConfig is the automatic recall: before a turn, the archive records most related to the
// message are put in front of it in that turn's requests (never the system prompt, never the
// session file).
type RecallConfig struct {
	// Auto: recall for user turns (nil: on). false turns it off; the tools still work.
	Auto *bool `json:"auto,omitempty" yaml:"auto,omitempty"`
	// Jobs: recall for scheduled runs too (their prompts are fixed; off by default).
	Jobs bool `json:"jobs,omitempty" yaml:"jobs,omitempty"`
	// Max records (1 to 10) and Budget in UTF-16 units of the recalled lines (200 to 4000) a turn
	// gets; 0: the runtime's 5 and 1200.
	Max    int `json:"max,omitempty" yaml:"max,omitempty"`
	Budget int `json:"budget,omitempty" yaml:"budget,omitempty"`
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
	// WorkGID is the work area's group: work/ is the runtime's, mode 0710, in this group, so
	// only a tainted run's commands (started with it as a supplementary group) reach their own
	// work/<conversation>/ (the jail grants no other), and no trusted command reaches any.
	WorkGID = 10002
)
