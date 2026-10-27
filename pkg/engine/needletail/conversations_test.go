package needletail

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/engine"
)

// fakeAPI is the needletail session API as Conversations uses it: c1 was compacted into c1b.
func fakeAPI(t *testing.T, status *int) (*httptest.Server, *[]string) {
	calls := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		if r.Header.Get("Authorization") != "Bearer test-key-0123456789" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if *status != 0 {
			w.WriteHeader(*status)
			_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			return
		}
		reply := func(code int, v any) { w.WriteHeader(code); _ = json.NewEncoder(w).Encode(v) }
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/v1/sessions":
			reply(200, wire.SessionList{Data: []wire.SessionSummary{
				{Session: wire.Session{V: 1, ID: "c1b", Parent: "c1", Created: "2026-10-12T08:00:00Z", Source: "api_server", Title: "Plan"}, Root: "c1", MessageCount: 3},
				{Session: wire.Session{V: 1, ID: "slack-C1-1", Source: "slack"}, Preview: "hi", MessageCount: 2, Ended: true},
			}})
		case r.Method == http.MethodGet && path == "/v1/sessions/c1/messages":
			if r.URL.Query().Get("order") != "latest" {
				t.Error("order")
			}
			reply(200, wire.Transcript{SessionID: "c1b", Data: []wire.Message{
				{Role: "system", Content: "prompt"},
				{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "call_1", Name: "terminal"}}},
				{Role: "tool", ToolCallID: "call_1", ToolName: "terminal", Content: strings.Repeat("x", toolOutputMax+10)},
				{Role: "assistant", Content: " Done. ", At: "2026-10-12T08:01:00Z"},
				{Role: "user", Content: ""},
			}})
		case r.Method == http.MethodPost && path == "/v1/sessions":
			var b wire.NewSession
			_ = json.NewDecoder(r.Body).Decode(&b)
			if b.ID == "c1" {
				reply(409, wire.Error{})
				return
			}
			reply(201, wire.Session{V: 1, ID: or(b.ID, "gen-1")})
		case r.Method == http.MethodPost && path == "/v1/sessions/new1/chat" && !strings.Contains(strings.Join(calls, ","), "POST /v1/sessions,"):
			reply(404, wire.Error{})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/chat"):
			body, _ := io.ReadAll(r.Body)
			var c wire.ChatRequest
			_ = json.Unmarshal(body, &c)
			tip := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/sessions/"), "/chat")
			if tip == "c1" {
				tip = "c1b"
			}
			out := wire.ChatResponse{SessionID: tip}
			out.Message.Content = "echo " + c.Message
			reply(200, out)
		case r.Method == http.MethodDelete && path == "/v1/sessions/c1":
			reply(200, wire.Deleted{Deleted: 2})
		default:
			reply(404, wire.Error{})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func client(srv *httptest.Server) engine.Conversations {
	call := func(ctx context.Context, method, path string, body any) (int, []byte, error) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = strings.NewReader(string(b))
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, rd)
		req.Header.Set("Authorization", "Bearer test-key-0123456789")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		return resp.StatusCode, raw, err
	}
	return NewRuntime().Conversations(call)
}

func TestConversations(t *testing.T) {
	status := 0
	srv, calls := fakeAPI(t, &status)
	c := client(srv)
	ctx := context.Background()

	list, err := c.List(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "c1" || !list[0].Compacted || list[0].Title != "Plan" || list[1].ID != "slack-C1-1" || list[1].Compacted || !list[1].Ended {
		t.Errorf("list = %+v", list)
	}

	tr, err := c.Transcript(ctx, "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Tip != "c1b" || !tr.Compacted || len(tr.Messages) != 3 {
		t.Fatalf("transcript = %+v", tr)
	}
	if m := tr.Messages[0]; m.Kind != engine.MessageTool || m.Tools[0] != "terminal" {
		t.Errorf("tool call row = %+v", m)
	}
	if m := tr.Messages[1]; m.Tool != "terminal" || len(m.Content) != toolOutputMax+len("…") {
		t.Errorf("tool row = %+v", m)
	}
	if m := tr.Messages[2]; m.Content != "Done." || m.Kind != engine.MessageText {
		t.Errorf("reply = %+v", m)
	}

	// new: 409 is the same conversation
	if id, err := c.New(ctx, "c1", ""); err != nil || id != "c1" {
		t.Errorf("new existing = %q, %v", id, err)
	}
	if id, err := c.New(ctx, "", "t"); err != nil || id != "gen-1" {
		t.Errorf("new generated = %q, %v", id, err)
	}

	// chat: the reply and the tip
	reply, tip, err := c.Send(ctx, "c1", "hi")
	if err != nil || reply != "echo hi" || tip != "c1b" {
		t.Errorf("send = %q %q %v", reply, tip, err)
	}
	// chat on a conversation that does not exist yet creates it
	*calls = nil
	reply, tip, err = c.Send(ctx, "new1", "hello")
	if err != nil || reply != "echo hello" || tip != "new1" {
		t.Errorf("send new = %q %q %v (%v)", reply, tip, err, *calls)
	}

	// delete: one request for the whole lineage
	*calls = nil
	if n, err := c.Delete(ctx, "c1"); err != nil || n != 2 || len(*calls) != 1 {
		t.Errorf("delete = %d %v %v", n, err, *calls)
	}
	if n, err := c.Delete(ctx, "gone"); err != nil || n != 0 {
		t.Errorf("delete missing = %d %v", n, err)
	}

	// status mapping
	for code, want := range map[int]error{404: engine.ErrNotFound, 409: engine.ErrBusy, 429: engine.ErrBusy, 503: engine.ErrBusy} {
		status = code
		if _, err := c.List(ctx, 5); !errors.Is(err, want) {
			t.Errorf("%d: %v", code, err)
		}
	}
	status = 500
	if _, err := c.List(ctx, 5); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("500: %v", err)
	}
}

// A follow-up's turn input is a user row no one wrote: the adapter marks it (Source), so a
// client labels it rather than showing it as the owner's message.
func TestFollowUpRowsAreMarked(t *testing.T) {
	if m, ok := message(wire.Message{Role: "user", Content: "[Approval ap_1 — a runtime note …]", Kind: wire.KindApproval}); !ok || m.Role != "assistant" {
		t.Errorf("an approval note shows as the person's: %+v", m)
	}
	if m, ok := message(wire.Message{Role: "user", Content: "[Follow-up fu1 …]\nx", Kind: wire.KindFollowUp}); !ok || m.Source != engine.SourceFollowUp || m.Role != "user" {
		t.Errorf("follow-up row %+v", m)
	}
	if m, _ := message(wire.Message{Role: "user", Content: "hello"}); m.Source != "" {
		t.Errorf("a person's row %+v", m)
	}
}
