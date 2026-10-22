// Package slacktest is a fake Slack for tests: the Web API methods the gateway calls (on an
// httptest server, a loopback URL) and a Socket Mode websocket that sends the envelopes a test
// gives it and records the gateway's acks. Nothing in it talks to Slack. Ids are made up
// (UBOT, U0ALLOWED, C0HOME…).
package slacktest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Tokens the fake accepts; they are not Slack tokens.
const (
	BotToken = "xoxb-fake-bot-token-for-tests"
	AppToken = "xapp-fake-app-token-for-tests"
	BotUser  = "U0BOTUSER1"
	BotID    = "B0BOTID001"
	Team     = "T0TEAM0001"
)

// Call is one Web API call the fake received.
type Call struct {
	Method string
	Params map[string]string
}

// Post is one chat.postMessage.
type Post struct {
	Channel, Thread, Text, TS string
}

// Server is the fake Slack.
type Server struct {
	*httptest.Server
	t *testing.T

	mu     sync.Mutex
	calls  []Call
	posts  []Post
	acks   []string
	opens  int
	conn   *websocket.Conn
	wmu    sync.Mutex
	nextTS int
	connCh chan struct{}
	// Names are users.info's display names; Replies conversations.replies' threads
	// (channel:thread_ts → messages); Fail makes a method answer {"ok":false,"error":code}.
	Names   map[string]string
	Replies map[string][]map[string]any
	Fail    map[string]string
	// RateLimit makes the next call of a method answer 429 with Retry-After 1.
	RateLimit map[string]bool
}

// New starts a fake Slack; Close stops it (registered with t.Cleanup).
func New(t *testing.T) *Server {
	s := &Server{t: t, Names: map[string]string{}, Replies: map[string][]map[string]any{}, Fail: map[string]string{},
		RateLimit: map[string]bool{}, connCh: make(chan struct{}, 16), nextTS: 1}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.api)
	mux.HandleFunc("/link", s.link)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// API is the Web API base URL to give the gateway.
func (s *Server) API() string { return s.URL + "/api" }

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	_ = r.ParseForm()
	params := map[string]string{}
	for k := range r.PostForm {
		params[k] = r.PostForm.Get(k)
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: method, Params: params})
	limited := s.RateLimit[method]
	delete(s.RateLimit, method)
	fail := s.Fail[method]
	s.mu.Unlock()
	reply := func(v map[string]any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	if limited {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	want := BotToken
	if method == "apps.connections.open" {
		want = AppToken
	}
	if token != want {
		reply(map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	if fail != "" {
		reply(map[string]any{"ok": false, "error": fail})
		return
	}
	switch method {
	case "auth.test":
		reply(map[string]any{"ok": true, "user_id": BotUser, "bot_id": BotID, "team_id": Team, "user": "agent"})
	case "apps.connections.open":
		s.mu.Lock()
		s.opens++
		n := s.opens
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "url": fmt.Sprintf("ws%s/link?n=%d", strings.TrimPrefix(s.URL, "http"), n)})
	case "chat.postMessage":
		s.mu.Lock()
		ts := fmt.Sprintf("1800000000.%06d", s.nextTS)
		s.nextTS++
		s.posts = append(s.posts, Post{Channel: params["channel"], Thread: params["thread_ts"], Text: params["text"], TS: ts})
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "channel": params["channel"], "ts": ts})
	case "users.info":
		name := s.Names[params["user"]]
		reply(map[string]any{"ok": true, "user": map[string]any{"id": params["user"], "name": strings.ToLower(name), "profile": map[string]any{"display_name": name}}})
	case "conversations.replies":
		s.mu.Lock()
		msgs := s.Replies[params["channel"]+":"+params["ts"]]
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "messages": msgs})
	case "conversations.open":
		reply(map[string]any{"ok": true, "channel": map[string]any{"id": "D0" + strings.TrimPrefix(params["users"], "U")}})
	case "chat.update", "reactions.add", "reactions.remove", "assistant.threads.setStatus":
		reply(map[string]any{"ok": true})
	default:
		reply(map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func (s *Server) link(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	s.write(conn, map[string]any{"type": "hello", "num_connections": 1})
	s.connCh <- struct{}{}
	for {
		_, body, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var ack struct {
			EnvelopeID string `json:"envelope_id"`
		}
		if json.Unmarshal(body, &ack) == nil && ack.EnvelopeID != "" {
			s.mu.Lock()
			s.acks = append(s.acks, ack.EnvelopeID)
			s.mu.Unlock()
		}
	}
}

func (s *Server) write(conn *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_ = conn.WriteMessage(websocket.TextMessage, b)
}

// WaitConnected waits for the gateway's next Socket Mode connection.
func (s *Server) WaitConnected(timeout time.Duration) {
	s.t.Helper()
	select {
	case <-s.connCh:
	case <-time.After(timeout):
		s.t.Fatal("slacktest: the gateway did not connect")
	}
}

// Event is a message event as Slack sends it.
type Event struct {
	Type, Subtype, Channel, ChannelType, User, BotID, Text, TS, ThreadTS string
}

// Send sends an events_api envelope with the event, as envelope id.
func (s *Server) Send(id string, e Event) {
	s.t.Helper()
	if e.Type == "" {
		e.Type = "message"
	}
	ev := map[string]any{"type": e.Type, "channel": e.Channel, "user": e.User, "text": e.Text, "ts": e.TS, "team": Team}
	for k, v := range map[string]string{"subtype": e.Subtype, "channel_type": e.ChannelType, "bot_id": e.BotID, "thread_ts": e.ThreadTS} {
		if v != "" {
			ev[k] = v
		}
	}
	env := map[string]any{"envelope_id": id, "type": "events_api", "accepts_response_payload": false,
		"payload": map[string]any{"team_id": Team, "event_id": "Ev" + id, "type": "event_callback", "event": ev}}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		s.t.Fatal("slacktest: no connection")
	}
	s.write(conn, env)
}

// Disconnect sends Slack's disconnect message (reason refresh_requested, say) and closes.
func (s *Server) Disconnect(reason string) {
	s.mu.Lock()
	conn := s.conn
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		s.write(conn, map[string]any{"type": "disconnect", "reason": reason})
		time.Sleep(20 * time.Millisecond)
		conn.Close()
	}
}

// Posts are the messages posted so far.
func (s *Server) Posts() []Post {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Post{}, s.posts...)
}

// WaitPosts waits until at least n messages are posted and returns them.
func (s *Server) WaitPosts(n int, timeout time.Duration) []Post {
	s.t.Helper()
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if p := s.Posts(); len(p) >= n {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Fatalf("slacktest: %d posts after %s, want %d: %+v", len(s.Posts()), timeout, n, s.Posts())
	return nil
}

// Calls are the Web API calls so far (a method's only, when method is set).
func (s *Server) Calls(method string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Call{}
	for _, c := range s.calls {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// Acks are the envelope ids acked so far.
func (s *Server) Acks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.acks...)
}

// SetFail makes method answer {"ok":false,"error":code} ("" stops it), under the fake's lock.
func (s *Server) SetFail(method, code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == "" {
		delete(s.Fail, method)
		return
	}
	s.Fail[method] = code
}

// Opens is how many times the gateway asked for a Socket Mode URL.
func (s *Server) Opens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opens
}
