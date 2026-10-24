package history

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/tools"
)

// ToolName is session_search's name (Hermes').
const ToolName = "session_search"

// Tool is session_search over ix (docs/needletail.md §4.5): Hermes' name and arguments (but its
// profile and detail), in four shapes picked by the arguments. audience is the calling turn's
// visibility (from its session, the runtime's: never the model's arguments); the turn's own
// session is never a hit.
func Tool(ix *Index, audience func(session string) Visible) tools.Tool {
	return tools.Tool{
		Name: ToolName,
		Description: "Recall your past conversations: search them by keyword, or read inside one. query: the conversations " +
			"whose messages hold the words (synonyms and both languages help; \"a phrase\", word*), each with its session_id, " +
			"date, the matching message (match_message_id), a snippet, and the messages around the best hit. " +
			"session_id with around_message_id: the messages around that one (scroll); session_id alone: that session read. " +
			"No arguments: your recent conversations. You find only conversations with the same people as this one " +
			"(this DM's person, this channel), never others'; this conversation's current messages are already in your context. " +
			"Use it for what was said or decided before; a file, a skill or a live system you can check directly comes first.",
		Schema: json.RawMessage(`{"type":"object","properties":{
"query":{"type":"string","description":"Words to find (ORed; \"a phrase\" as written, word* a prefix). Omit to browse recent conversations."},
"limit":{"type":"integer","minimum":1,"maximum":10,"description":"Conversations to answer (default 3)."},
"sort":{"type":"string","enum":["newest","oldest"],"description":"Omit for relevance; newest for where something was left, oldest for how it started."},
"after":{"type":"string","description":"Sessions started at or after: an ISO date or time, or a duration back (7d, 24h, 2w)."},
"before":{"type":"string","description":"Sessions started before: an ISO date or time, or a duration back (7d = older than a week)."},
"source":{"type":"string","enum":["slack","api_server","cron","cli"],"description":"Only conversations of this source."},
"role_filter":{"type":"string","description":"user, assistant or user,assistant (default)."},
"exclude_session_ids":{"type":"array","items":{"type":"string"},"maxItems":20,"description":"Sessions already looked at (their conversations are left out)."},
"session_id":{"type":"string","description":"A session to read (alone) or scroll (with around_message_id)."},
"around_message_id":{"type":"integer","minimum":0,"description":"With session_id: the message to centre on (a hit's match_message_id, or an id from a window)."},
"window":{"type":"integer","minimum":1,"maximum":20,"description":"With around_message_id: messages on each side (default 5)."}}}`),
		Run: func(_ context.Context, env *tools.Env, raw json.RawMessage) (any, error) {
			var a struct {
				Query     string   `json:"query"`
				Limit     int      `json:"limit"`
				Sort      string   `json:"sort"`
				After     string   `json:"after"`
				Before    string   `json:"before"`
				Source    string   `json:"source"`
				Roles     string   `json:"role_filter"`
				Exclude   []string `json:"exclude_session_ids"`
				SessionID string   `json:"session_id"`
				Around    *int     `json:"around_message_id"`
				Window    int      `json:"window"`
			}
			if err := json.Unmarshal(lenient(raw), &a); err != nil {
				return nil, fmt.Errorf("bad arguments: %v", err)
			}
			session := ""
			if env != nil {
				session = env.Session
			}
			var visible Visible
			if audience != nil {
				visible = audience(session)
			}
			if visible == nil {
				visible = func(string, string) bool { return false }
			}
			if id := sessionID(a.SessionID); id != "" {
				return ix.Scroll(Scroll{SessionID: id, Around: a.Around, Window: a.Window, Current: session, Visible: visible})
			}
			if strings.TrimSpace(a.Query) == "" {
				return ix.Browse(Browse{Limit: a.Limit, Source: a.Source, Current: session, Visible: visible})
			}
			return ix.Search(Query{Text: a.Query, Limit: a.Limit, Sort: strings.ToLower(strings.TrimSpace(a.Sort)), After: a.After, Before: a.Before,
				Source: a.Source, Roles: a.Roles, Exclude: a.Exclude, Current: session, Visible: visible})
		},
		HookArgs: hookArgs,
	}
}

// sessionID is the session a call names: an id, or Hermes' @session:<profile>/<id> link.
func sessionID(s string) string {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@session:"))
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// lenient takes numbers sent as strings ("around_message_id": "12"), as models send them.
func lenient(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return raw
	}
	changed := false
	for _, k := range []string{"limit", "around_message_id", "window"} {
		if v, ok := m[k].(string); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				m[k] = n
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	b, _ := json.Marshal(m)
	return b
}

// hookArgs is a call as observers (the core's hooks) see it: the shape, enums, numbers and
// whether a text was given; never the query, a session id or a time bound's text (a query is
// client data).
func hookArgs(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || m == nil {
		return "{}"
	}
	out := map[string]any{}
	for k, allowed := range map[string][]string{"sort": {"newest", "oldest"}, "source": {"slack", "api_server", "cron", "cli"}} {
		if v, ok := m[k].(string); ok {
			if slices.Contains(allowed, v) {
				out[k] = v
			} else {
				out[k] = "other"
			}
		}
	}
	for _, k := range []string{"limit", "around_message_id", "window"} {
		if v, ok := m[k].(float64); ok {
			out[k] = v
		}
	}
	for _, k := range []string{"query", "session_id", "after", "before", "role_filter"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			out["has_"+k] = true
		}
	}
	if v, ok := m["exclude_session_ids"].([]any); ok {
		out["exclude_session_ids_count"] = len(v)
	}
	b, _ := json.Marshal(out)
	return string(b)
}
