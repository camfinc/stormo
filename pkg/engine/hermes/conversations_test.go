package hermes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine"
)

// fakeSessions is Hermes' session API as far as Conversations uses it: c1 was compacted into c1b,
// a chat on a compacted root would answer from the root's own history (so it must not be used).
type fakeSessions struct {
	sessions map[string]map[string]any
	messages map[string][]map[string]any
	tip      map[string]string
	busy     bool
	calls    []string
}

func newFake() *fakeSessions {
	return &fakeSessions{
		sessions: map[string]map[string]any{
			"c1":    {"id": "c1", "title": "Plan", "source": "api_server", "started_at": 1.7e9, "message_count": 0, "ended_at": 1.7e9 + 60},
			"c1b":   {"id": "c1b", "source": "api_server", "parent_session_id": "c1", "message_count": 3},
			"slack": {"id": "slack", "source": "slack", "preview": "hi", "message_count": 2},
		},
		messages: map[string][]map[string]any{
			"c1b": {
				{"id": 1, "role": "user", "content": "", "display_kind": "hidden"},
				{"id": 2, "role": "assistant", "content": "", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "web_search"}}}},
				{"id": 3, "role": "tool", "tool_name": "web_search", "content": strings.Repeat("x", toolOutputMax+10)},
				{"id": 4, "role": "assistant", "content": []any{map[string]any{"type": "text", "text": "Done."}}, "timestamp": 1.7e9},
				{"id": 5, "role": "system", "content": "prompt"},
			},
		},
		tip: map[string]string{"c1": "c1b"},
	}
}

func (f *fakeSessions) call(_ context.Context, method, path string, body any) (int, []byte, error) {
	f.calls = append(f.calls, method+" "+path)
	u, _ := url.Parse(path)
	parts := strings.Split(strings.TrimPrefix(u.Path, "/api/sessions"), "/")
	reply := func(status int, v any) (int, []byte, error) { b, _ := json.Marshal(v); return status, b, nil }
	id := ""
	if len(parts) > 1 {
		id = parts[1]
	}
	switch {
	case method == http.MethodGet && id == "":
		var data []any
		for _, s := range f.sessions {
			if s["parent_session_id"] == nil || u.Query().Get("include_children") == "true" {
				data = append(data, s)
			}
		}
		return reply(200, map[string]any{"data": data})
	case method == http.MethodPost && id == "":
		b := body.(map[string]any)
		sid, _ := b["id"].(string)
		if sid == "" {
			sid = "api_new"
		}
		if f.sessions[sid] != nil {
			return reply(409, map[string]any{"error": map[string]any{"message": "exists"}})
		}
		f.sessions[sid] = map[string]any{"id": sid, "source": b["source"]}
		return reply(201, map[string]any{"session": f.sessions[sid]})
	case f.sessions[id] == nil:
		return reply(404, map[string]any{"error": map[string]any{"message": "Session not found"}})
	case method == http.MethodGet && len(parts) == 3 && parts[2] == "messages":
		tip := id
		if t := f.tip[id]; t != "" {
			tip = t
		}
		return reply(200, map[string]any{"session_id": tip, "data": f.messages[tip]})
	case method == http.MethodPost && len(parts) == 3 && parts[2] == "chat":
		if f.busy {
			return reply(429, map[string]any{"error": map[string]any{"message": "busy"}})
		}
		msg := body.(map[string]any)["message"]
		f.messages[id] = append(f.messages[id], map[string]any{"role": "user", "content": msg})
		return reply(200, map[string]any{"session_id": id, "message": map[string]any{"role": "assistant", "content": "echo " + msg.(string)}})
	case method == http.MethodDelete:
		delete(f.sessions, id)
		return reply(200, map[string]any{"deleted": true})
	}
	return reply(400, nil)
}

func TestConversations(t *testing.T) {
	ctx := context.Background()
	f := newFake()
	c := NewRuntime().Conversations(f.call)

	list, err := c.List(ctx, 0)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v (continuations are not conversations)", list, err)
	}
	for _, cv := range list {
		if cv.ID == "c1" && (cv.Title != "Plan" || !cv.Ended || cv.StartedAt == "") {
			t.Errorf("c1 = %+v", cv)
		}
	}

	tr, err := c.Transcript(ctx, "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.Compacted || tr.Tip != "c1b" || len(tr.Messages) != 3 {
		t.Fatalf("transcript = %+v", tr)
	}
	if m := tr.Messages[0]; m.Kind != engine.MessageTool || m.Tools[0] != "web_search" {
		t.Errorf("tool call row = %+v", m)
	}
	if m := tr.Messages[1]; m.Tool != "web_search" || len(m.Content) > toolOutputMax+len("…") {
		t.Errorf("tool output row = %d bytes", len(m.Content))
	}
	if m := tr.Messages[2]; m.Content != "Done." || m.At == "" {
		t.Errorf("reply row = %+v", m)
	}

	// A turn on a compacted conversation goes to its tip.
	reply, tip, err := c.Send(ctx, "c1", "next")
	if err != nil || reply != "echo next" || tip != "c1b" {
		t.Fatalf("send = %q %q %v", reply, tip, err)
	}
	if last := f.calls[len(f.calls)-1]; last != "POST /api/sessions/c1b/chat" {
		t.Errorf("chat went to %s", last)
	}

	// A conversation that does not exist yet is created (stormo chat's default session).
	if _, tip, err := c.Send(ctx, "swarm-local", "hi"); err != nil || tip != "swarm-local" || f.sessions["swarm-local"] == nil {
		t.Fatalf("send to new = %q %v", tip, err)
	}

	f.busy = true
	if _, _, err := c.Send(ctx, "c1", "again"); !errors.Is(err, engine.ErrBusy) {
		t.Errorf("busy = %v", err)
	}

	if _, err := c.Transcript(ctx, "nope", 0); !errors.Is(err, engine.ErrNotFound) {
		t.Errorf("missing = %v", err)
	}

	// Clearing takes the continuation with it.
	n, err := c.Delete(ctx, "c1")
	if err != nil || n != 2 || f.sessions["c1"] != nil || f.sessions["c1b"] != nil || f.sessions["slack"] == nil {
		t.Fatalf("delete = %d %v, left %v", n, err, f.sessions)
	}
	if n, err := c.Delete(ctx, "c1"); err != nil || n != 0 {
		t.Errorf("second delete = %d %v", n, err)
	}

	if id, err := c.New(ctx, "", "Title"); err != nil || id != "api_new" {
		t.Errorf("new = %q %v", id, err)
	}
}
