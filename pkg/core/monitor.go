package core

import (
	"io"
	"log"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/loop"
)

// Activity ingest (docs/core.md §2, phase 3). Each local agent's engine posts its lifecycle hooks
// (Hermes: `hooks.outbound`, wired by pkg/engine/hermes for agents on the core) to
// POST /ingest/<engine kind>, signed with the agent's own core key; the engine's runtime reads them
// (engine.Runtime.ParseHook). The core keeps a live view per agent (tool calls in
// flight, open sessions, approvals waiting) and a timeline in core.db. Previews can hold client
// data: they stay in core.db for RetainActivity and only reach owner-authenticated reads.

const (
	// RetainActivity is how long timeline rows (and their previews) are kept.
	RetainActivity = 7 * 24 * time.Hour
	// A delivery older (or newer) than this is refused: the timestamp is inside the signed body.
	ingestSkew = 10 * time.Minute
	// A tool call whose post never arrived (delivery is best effort) stops counting after this.
	callTimeout = 15 * time.Minute
	// An open session with no event for this long is forgotten.
	sessionTimeout = 6 * time.Hour
	maxIngestBody  = 8 << 20
	maxPreview     = 160
)

// LiveCall is a tool call in flight.
type LiveCall struct {
	Tool  string `json:"tool"`
	Since string `json:"since"`
	// Path is the shared-space file a file tool is working on (workdir attribution), never sent out.
	path  string
	since int64
	sess  string
}

// Live is what an agent is doing right now, from its hooks. Tool names and times only.
type Live struct {
	// The newest tool call still running, and since when.
	Tool      string `json:"tool,omitempty"`
	ToolSince string `json:"toolSince,omitempty"`
	Running   int    `json:"running"`
	Sessions  int    `json:"sessions"`
	// Approvals the agent is waiting on (a person must answer).
	Waiting   int    `json:"waiting"`
	LastEvent string `json:"lastEvent,omitempty"`
	LastTool  string `json:"lastTool,omitempty"`
}

type agentLive struct {
	calls    map[string]*LiveCall
	sessions map[string]int64
	waiting  map[string]int64
	last     int64
	lastTool string
}

// TimelineEntry is one row of an agent's activity. Preview and Session only reach owner reads.
type TimelineEntry struct {
	At      string `json:"at"`
	Event   string `json:"event"`
	Tool    string `json:"tool,omitempty"`
	Session string `json:"session,omitempty"`
	Preview string `json:"preview,omitempty"`
}

// ToolEvent is a finished file-tool call, for workdir attribution.
type ToolEvent struct {
	Agent string
	Tool  string
	Path  string // as the agent named it (container path)
	At    int64
}

// Monitor ingests hook deliveries; safe for concurrent use.
type Monitor struct {
	db    *DB
	keys  *AgentKeys
	scrub func(string) string
	now   func() time.Time

	mu   sync.Mutex
	live map[string]*agentLive
	subs []func(ToolEvent)
}

// NewMonitor reads deliveries signed with keys and records them in db. scrub redacts previews
// (nil: the instance's learning scrubber is not applied, tests only).
func NewMonitor(db *DB, keys *AgentKeys, scrub func(string) string) *Monitor {
	if scrub == nil {
		scrub = func(s string) string { return s }
	}
	return &Monitor{db: db, keys: keys, scrub: scrub, now: time.Now, live: map[string]*agentLive{}}
}

// Scrubber is the instance's PII and secret scrubber as a preview filter.
func Scrubber(s *learning.Scrubber) func(string) string {
	return func(in string) string { out, _ := s.Scrub(in); return out }
}

// OnToolDone registers f for every finished tool call that named a shared-space path.
func (m *Monitor) OnToolDone(f func(ToolEvent)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs = append(m.subs, f)
}

// Handle serves POST /ingest/<engine kind>. Unsigned, unknown-key, stale or malformed deliveries
// are refused with 4xx (engines do not retry those); a duplicate delivery is accepted and ignored.
func (m *Monitor) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONBody(w, 405, errBody("method_not_allowed", "POST only"))
		return
	}
	rt, err := engines.Runtime(strings.TrimPrefix(r.URL.Path, "/ingest/"))
	if err != nil {
		writeJSONBody(w, 404, errBody("not_found", "no such engine"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxIngestBody+1))
	if err != nil {
		writeJSONBody(w, 400, errBody("bad_request", "unreadable body"))
		return
	}
	if len(body) > maxIngestBody {
		writeJSONBody(w, 413, errBody("too_large", "delivery too large"))
		return
	}
	agent, ok := m.keys.VerifySignature(body, rt.HookSignature(r.Header))
	if !ok {
		writeJSONBody(w, 401, errBody("unauthorized", "missing or unknown signature"))
		return
	}
	e, err := rt.ParseHook(body)
	if err != nil {
		writeJSONBody(w, 400, errBody("bad_request", err.Error()))
		return
	}
	now := m.now()
	if e.At.Before(now.Add(-ingestSkew)) || e.At.After(now.Add(ingestSkew)) {
		writeJSONBody(w, 400, errBody("stale", "timestamp missing or outside the accepted window"))
		return
	}
	if err := m.Ingest(agent, e); err != nil {
		log.Printf("ingest %s: %v", agent, err)
		writeJSONBody(w, 500, errBody("core_internal", "could not record the event"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

var pathKeys = []string{"path", "file_path", "filepath", "target", "filename"}

// toolPath is the file a tool call names, if any.
func toolPath(in map[string]any) string {
	for _, k := range pathKeys {
		if s, ok := in[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

var spaces = regexp.MustCompile(`\s+`)

func clip(s string, n int) string {
	s = strings.TrimSpace(spaces.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// preview is a short line about a hook: the file a tool touches, the start of a terminal command,
// else the names of the arguments (never their values). Scrubbed before it is stored.
func (m *Monitor) preview(e *engine.HookEvent) string {
	switch e.Kind {
	case engine.ToolStart:
		if p := toolPath(e.ToolInput); p != "" {
			return m.scrub(clip(p, maxPreview))
		}
		if c, ok := e.ToolInput["command"].(string); ok {
			return m.scrub(clip(c, maxPreview))
		}
		keys := []string{}
		for k := range e.ToolInput {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return clip(strings.Join(keys, ", "), maxPreview)
	case engine.ToolDone:
		s := e.Status
		if e.Duration > 0 {
			s = strings.TrimSpace(s + " " + e.Duration.Round(time.Millisecond).String())
		}
		return clip(s, maxPreview)
	case engine.SessionStart, engine.SessionEnd:
		return clip(e.Platform, 40)
	}
	return ""
}

// Ingest records one verified delivery from agent.
func (m *Monitor) Ingest(agent string, e *engine.HookEvent) error {
	at := e.At
	ms := at.UnixMilli()
	tool := clip(e.Tool, 80)
	res, err := m.db.Exec(`INSERT OR IGNORE INTO activity(agent, at, event, session, tool, call, preview, delivery) VALUES(?,?,?,?,?,?,?,?)`,
		agent, ms, clip(e.Name, 40), clip(e.Session, 120), tool, clip(e.CallID, 120), m.preview(e), e.Delivery)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // a retried delivery: already counted
	}
	var done *ToolEvent
	m.mu.Lock()
	l := m.live[agent]
	if l == nil {
		l = &agentLive{calls: map[string]*LiveCall{}, sessions: map[string]int64{}, waiting: map[string]int64{}}
		m.live[agent] = l
	}
	l.last = ms
	if e.Session != "" && e.Kind != engine.SessionEnd {
		l.sessions[e.Session] = ms
	}
	switch e.Kind {
	case engine.SessionEnd:
		delete(l.sessions, e.Session)
		for k, c := range l.calls {
			if c.sess == e.Session {
				delete(l.calls, k)
			}
		}
	case engine.ToolStart:
		l.calls[e.CallID] = &LiveCall{Tool: tool, Since: loop.IsoMillis(at), path: toolPath(e.ToolInput), since: ms, sess: e.Session}
		l.lastTool = tool
	case engine.ToolDone:
		key := e.CallID
		p := toolPath(e.ToolInput)
		if c := l.calls[key]; c != nil && p == "" {
			p = c.path
		}
		delete(l.calls, key)
		if p != "" {
			done = &ToolEvent{Agent: agent, Tool: tool, Path: p, At: ms}
		}
	case engine.ApprovalWaiting:
		l.waiting[e.ApprovalID] = ms
	case engine.ApprovalAnswered:
		delete(l.waiting, e.ApprovalID)
	}
	subs := append([]func(ToolEvent){}, m.subs...)
	m.mu.Unlock()
	if done != nil {
		for _, f := range subs {
			f(*done)
		}
	}
	return nil
}

// expire drops calls, sessions and approvals whose closing event never came. Caller holds m.mu.
func (m *Monitor) expire(l *agentLive, now int64) {
	for k, c := range l.calls {
		if now-c.since > callTimeout.Milliseconds() {
			delete(l.calls, k)
		}
	}
	for k, t := range l.sessions {
		if now-t > sessionTimeout.Milliseconds() {
			delete(l.sessions, k)
		}
	}
	for k, t := range l.waiting {
		if now-t > sessionTimeout.Milliseconds() {
			delete(l.waiting, k)
		}
	}
}

// Live is the agent's live state, nil when it never posted a hook.
func (m *Monitor) Live(agent string) *Live {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.live[agent]
	if l == nil {
		return nil
	}
	m.expire(l, m.now().UnixMilli())
	out := &Live{Running: len(l.calls), Sessions: len(l.sessions), Waiting: len(l.waiting), LastEvent: loop.IsoMillis(time.UnixMilli(l.last)), LastTool: l.lastTool}
	var newest *LiveCall
	for _, c := range l.calls {
		if newest == nil || c.since > newest.since {
			newest = c
		}
	}
	if newest != nil {
		out.Tool, out.ToolSince = newest.Tool, newest.Since
	}
	return out
}

// RunningCalls are an agent's tool calls in flight, with the path each names (workdir attribution).
func (m *Monitor) RunningCalls(agent string) []LiveCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	l := m.live[agent]
	if l == nil {
		return nil
	}
	m.expire(l, m.now().UnixMilli())
	out := []LiveCall{}
	for _, c := range l.calls {
		out = append(out, *c)
	}
	return out
}

// Forget drops an agent's live state (it went down: its open calls will never finish).
func (m *Monitor) Forget(agent string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.live, agent)
}

// Timeline is an agent's newest activity rows, newest first. private adds sessions and previews.
func (m *Monitor) Timeline(agent string, limit int, private bool) ([]TimelineEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := m.db.Query(`SELECT at, event, tool, session, preview FROM activity WHERE agent = ? ORDER BY at DESC, id DESC LIMIT ?`, agent, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TimelineEntry{}
	for rows.Next() {
		var at int64
		var e TimelineEntry
		if err := rows.Scan(&at, &e.Event, &e.Tool, &e.Session, &e.Preview); err != nil {
			return nil, err
		}
		e.At = loop.IsoMillis(time.UnixMilli(at))
		if !private {
			e.Session, e.Preview = "", ""
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Prune deletes activity older than RetainActivity.
func (m *Monitor) Prune() error {
	_, err := m.db.Exec(`DELETE FROM activity WHERE at < ?`, m.now().Add(-RetainActivity).UnixMilli())
	return err
}

// cleanPath normalises a container path ("/shared/group/a/../b.md" → "/shared/group/b.md").
func cleanPath(p string) string { return path.Clean("/" + strings.TrimPrefix(p, "/")) }
