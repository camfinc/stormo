package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
)

// Conversations are Hermes' sessions (`/api/sessions`). A Hermes compaction ends the session and
// continues the conversation in a child (`end_reason: compression`). The listing shows a chain as
// its live tip (`_lineage_root_id` naming the first session); `/messages` follows the chain from
// any of its sessions, `/chat` does not. So a conversation's id is its root, a turn goes to the
// tip, and a delete takes the whole chain.
func (runtime) Conversations(call engine.APICall) engine.Conversations { return conversations{call} }

type conversations struct{ call engine.APICall }

// toolOutputMax bounds a tool row's text: tool output can be whole files.
const toolOutputMax = 4000

type hermesSession struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Source       string `json:"source"`
	Preview      string `json:"preview"`
	StartedAt    any    `json:"started_at"`
	LastActive   any    `json:"last_active"`
	EndedAt      any    `json:"ended_at"`
	MessageCount int    `json:"message_count"`
	Parent       string `json:"parent_session_id"`
	EndReason    string `json:"end_reason"`
	Root         string `json:"_lineage_root_id"`
	Hidden       bool   `json:"hidden"`
}

func (c conversations) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	status, raw, err := c.call(ctx, method, path, body)
	if err != nil {
		return 0, err
	}
	switch {
	case status == http.StatusNotFound:
		return status, engine.ErrNotFound
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
		return status, engine.ErrBusy
	case status < 200 || status > 299:
		return status, fmt.Errorf("%s %s: HTTP %d %s", method, path, status, apiError(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return status, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return status, nil
}

// apiError is the message of Hermes' `{"error":{"message"}}`, else the body's start.
func apiError(raw []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func sessionPath(id string) string { return "/api/sessions/" + url.PathEscape(id) }

func (c conversations) sessions(ctx context.Context, limit int, children bool) ([]hermesSession, error) {
	q := url.Values{"limit": {fmt.Sprint(limit)}}
	if children {
		q.Set("include_children", "true")
	}
	var out struct {
		Data []hermesSession `json:"data"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/api/sessions?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

func (c conversations) List(ctx context.Context, limit int) ([]engine.Conversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := c.sessions(ctx, limit, false)
	if err != nil {
		return nil, err
	}
	list := []engine.Conversation{}
	for _, s := range rows {
		if s.Hidden || s.ID == "" {
			continue
		}
		id := s.ID
		if s.Root != "" {
			id = s.Root
		}
		list = append(list, engine.Conversation{
			ID: id, Title: s.Title, Source: s.Source, Preview: s.Preview,
			StartedAt: when(s.StartedAt), LastActive: when(s.LastActive),
			Messages: s.MessageCount, Ended: when(s.EndedAt) != "", Compacted: id != s.ID,
		})
	}
	return list, nil
}

type hermesMessage struct {
	ID         any    `json:"id"`
	Role       string `json:"role"`
	Content    any    `json:"content"`
	ToolName   string `json:"tool_name"`
	Timestamp  any    `json:"timestamp"`
	Display    string `json:"display_kind"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func (c conversations) Transcript(ctx context.Context, id string, limit int) (*engine.Transcript, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var out struct {
		SessionID string          `json:"session_id"`
		Data      []hermesMessage `json:"data"`
	}
	path := sessionPath(id) + "/messages?" + url.Values{"limit": {fmt.Sprint(limit)}, "order": {"latest"}}.Encode()
	if _, err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	tip := out.SessionID
	if tip == "" {
		tip = id
	}
	t := &engine.Transcript{ID: id, Tip: tip, Compacted: tip != id, Messages: []engine.Message{}}
	for _, m := range out.Data {
		if msg, ok := message(m); ok {
			t.Messages = append(t.Messages, msg)
		}
	}
	return t, nil
}

// message is a Hermes row as a visible message: compaction carriers and diagnostics (hidden), the
// system prompt and empty rows are not shown.
func message(m hermesMessage) (engine.Message, bool) {
	if m.Display == "hidden" || m.Role == "system" {
		return engine.Message{}, false
	}
	msg := engine.Message{Role: m.Role, Kind: engine.MessageText, Content: strings.TrimSpace(text(m.Content)), At: when(m.Timestamp)}
	if m.ID != nil {
		msg.ID = fmt.Sprint(m.ID)
	}
	for _, tc := range m.ToolCalls {
		if tc.Function.Name != "" {
			msg.Tools = append(msg.Tools, tc.Function.Name)
		}
	}
	if m.Role == "tool" {
		msg.Kind, msg.Tool = engine.MessageTool, m.ToolName
		if len(msg.Content) > toolOutputMax {
			msg.Content = msg.Content[:toolOutputMax] + "…"
		}
	} else if msg.Content == "" && len(msg.Tools) > 0 {
		msg.Kind = engine.MessageTool
	}
	if msg.Content == "" && len(msg.Tools) == 0 && msg.Kind == engine.MessageText {
		return engine.Message{}, false
	}
	return msg, true
}

// text is a message's content: a string, or the text parts of a multimodal list.
func text(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, p := range v {
			if m, ok := p.(map[string]any); ok {
				if s, ok := m["text"].(string); ok {
					parts = append(parts, s)
				} else if t, _ := m["type"].(string); strings.HasPrefix(t, "image") {
					parts = append(parts, "[image]")
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// when is a Hermes time (epoch seconds, or a string) as RFC 3339, "" when absent.
func when(v any) string {
	switch t := v.(type) {
	case float64:
		if t <= 0 {
			return ""
		}
		sec, frac := math.Modf(t)
		return time.Unix(int64(sec), int64(frac*1e9)).UTC().Format(time.RFC3339)
	case string:
		return t
	}
	return ""
}

func (c conversations) New(ctx context.Context, id, title string) (string, error) {
	body := map[string]any{"source": "api_server"}
	if id != "" {
		body["id"] = id
	}
	if title != "" {
		body["title"] = title
	}
	var out struct {
		Session hermesSession `json:"session"`
	}
	status, err := c.do(ctx, http.MethodPost, "/api/sessions", body, &out)
	if status == http.StatusConflict {
		return id, nil // already there: the same conversation
	}
	if err != nil {
		return "", err
	}
	return out.Session.ID, nil
}

func (c conversations) Send(ctx context.Context, id, message string) (string, string, error) {
	tip := id
	t, err := c.Transcript(ctx, id, 1)
	switch {
	case err == engine.ErrNotFound:
		if _, err := c.New(ctx, id, ""); err != nil {
			return "", "", err
		}
	case err != nil:
		return "", "", err
	default:
		tip = t.Tip
	}
	var out struct {
		SessionID string `json:"session_id"`
		Message   struct {
			Content any `json:"content"`
		} `json:"message"`
	}
	if _, err := c.do(ctx, http.MethodPost, sessionPath(tip)+"/chat", map[string]any{"message": message}, &out); err != nil {
		return "", "", err
	}
	if out.SessionID != "" {
		tip = out.SessionID
	}
	return text(out.Message.Content), tip, nil
}

func (c conversations) Delete(ctx context.Context, id string) (int, error) {
	rows, err := c.sessions(ctx, 200, true)
	if err != nil {
		return 0, err
	}
	byID, children := map[string]hermesSession{}, map[string][]string{}
	for _, s := range rows {
		byID[s.ID] = s
		if s.Parent != "" {
			children[s.Parent] = append(children[s.Parent], s.ID)
		}
	}
	// Up to the conversation's first session (id may be a later one of the chain)...
	for i := 0; i < 32; i++ {
		p, ok := byID[byID[id].Parent]
		if !ok || p.EndReason != "compression" {
			break
		}
		id = p.ID
	}
	// ...then it and everything that continued it (a delete orphans a child; the ids are collected).
	ids, seen := []string{id}, map[string]bool{id: true}
	for i := 0; i < len(ids); i++ {
		for _, ch := range children[ids[i]] {
			if !seen[ch] {
				seen[ch] = true
				ids = append(ids, ch)
			}
		}
	}
	n := 0
	for _, sid := range ids {
		var out struct {
			Deleted bool `json:"deleted"`
		}
		_, err := c.do(ctx, http.MethodDelete, sessionPath(sid), nil, &out)
		if err == engine.ErrNotFound {
			continue
		}
		if err != nil {
			return n, err
		}
		if out.Deleted {
			n++
		}
	}
	return n, nil
}
