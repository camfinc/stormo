package needletail

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/engine"
)

// Conversations are the runtime's sessions (/v1/sessions). As with Hermes, a compaction ends a
// session and continues it in a child; the listing shows a conversation as its tip with Root naming
// its first session, which is the conversation's id. Unlike Hermes, the runtime follows the lineage
// itself: /chat on any session of it goes to the tip, and DELETE removes the whole lineage.
func (runtime) Conversations(call engine.APICall) engine.Conversations { return conversations{call} }

type conversations struct{ call engine.APICall }

// toolOutputMax bounds a tool row's text: tool output can be whole files.
const toolOutputMax = 4000

func (c conversations) do(ctx context.Context, method, path string, body any, out any) (int, error) {
	status, raw, err := c.call(ctx, method, path, body)
	if err != nil {
		return 0, err
	}
	switch {
	case status == http.StatusNotFound:
		return status, engine.ErrNotFound
	case status == http.StatusConflict || status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
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

func apiError(raw []byte) string {
	var e wire.Error
	if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func sessionPath(id string) string { return "/v1/sessions/" + url.PathEscape(id) }

func (c conversations) List(ctx context.Context, limit int) ([]engine.Conversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	var out wire.SessionList
	if _, err := c.do(ctx, http.MethodGet, "/v1/sessions?"+url.Values{"limit": {fmt.Sprint(limit)}}.Encode(), nil, &out); err != nil {
		return nil, err
	}
	list := []engine.Conversation{}
	for _, s := range out.Data {
		if s.ID == "" {
			continue
		}
		id := s.ID
		if s.Root != "" {
			id = s.Root
		}
		list = append(list, engine.Conversation{ID: id, Title: s.Title, Source: s.Source, Preview: s.Preview,
			StartedAt: s.Created, LastActive: s.LastActive, Messages: s.MessageCount, Ended: s.Ended, Compacted: id != s.ID})
	}
	return list, nil
}

func (c conversations) Transcript(ctx context.Context, id string, limit int) (*engine.Transcript, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	var out wire.Transcript
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

// message is a session row as a visible message: the system prompt and empty rows are not shown.
func message(m wire.Message) (engine.Message, bool) {
	if m.Role == "system" {
		return engine.Message{}, false
	}
	msg := engine.Message{Role: m.Role, Kind: engine.MessageText, Content: strings.TrimSpace(m.Content), At: m.At}
	if m.Role == "user" && m.Kind == wire.KindFollowUp {
		msg.Source = engine.SourceFollowUp // the agent's own follow-up started this turn: no one wrote it
	}
	if m.Role == "user" && m.Kind == wire.KindApproval {
		msg.Role = "assistant" // the runtime's note of an approval's outcome: never shown as the person's words
	}
	for _, tc := range m.ToolCalls {
		if tc.Name != "" {
			msg.Tools = append(msg.Tools, tc.Name)
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

// New creates a session; 409 means it exists, which is the same conversation.
func (c conversations) New(ctx context.Context, id, title string) (string, error) {
	var out wire.Session
	status, err := c.do(ctx, http.MethodPost, "/v1/sessions", wire.NewSession{ID: id, Title: title, Source: "api_server"}, &out)
	if status == http.StatusConflict {
		return id, nil
	}
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

// Send posts one turn; the runtime routes it to the conversation's tip. A conversation that does
// not exist yet is created first.
func (c conversations) Send(ctx context.Context, id, message string) (string, string, error) {
	var out wire.ChatResponse
	_, err := c.do(ctx, http.MethodPost, sessionPath(id)+"/chat", wire.ChatRequest{Message: message}, &out)
	if err == engine.ErrNotFound {
		if _, err := c.New(ctx, id, ""); err != nil {
			return "", "", err
		}
		_, err = c.do(ctx, http.MethodPost, sessionPath(id)+"/chat", wire.ChatRequest{Message: message}, &out)
		if err != nil {
			return "", "", err
		}
	} else if err != nil {
		return "", "", err
	}
	tip := id
	if out.SessionID != "" {
		tip = out.SessionID
	}
	return out.Message.Content, tip, nil
}

// Delete removes the conversation's whole lineage in one request; 0 when it did not exist.
func (c conversations) Delete(ctx context.Context, id string) (int, error) {
	var out wire.Deleted
	_, err := c.do(ctx, http.MethodDelete, sessionPath(id), nil, &out)
	if err == engine.ErrNotFound {
		return 0, nil
	}
	return out.Deleted, err
}
