// Package slacktest is a fake Slack for tests: the Web API methods the gateway calls (on an
// httptest server, a loopback URL) and a Socket Mode websocket that sends the envelopes a test
// gives it and records the gateway's acks. Nothing in it talks to Slack. Ids are made up
// (UBOT, U0ALLOWED, C0HOME…).
package slacktest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
	// Blocks are the message's Block Kit blocks (an approval card's), as JSON; "" for none.
	Blocks string
}

// Update is one chat.update.
type Update struct {
	Channel, TS, Text, Blocks string
}

// Ephemeral is one chat.postEphemeral.
type Ephemeral struct {
	Channel, User, Text string
}

// WorkspaceURL is the workspace address auth.test gives (permalinks).
const WorkspaceURL = "https://acme-test.slack.com/"

// Server is the fake Slack.
type Server struct {
	*httptest.Server
	t *testing.T

	mu         sync.Mutex
	calls      []Call
	posts      []Post
	uploaded   map[string][]byte // file id → bytes sent to its upload URL
	shares     []Share
	updates    []Update
	ephemerals []Ephemeral
	acks       []string
	opens      int
	conn       *websocket.Conn
	wmu        sync.Mutex
	nextTS     int
	connCh     chan struct{}
	// Names are users.info's display names; Replies conversations.replies' threads
	// (channel:thread_ts → messages); Fail makes a method answer {"ok":false,"error":code}.
	Names   map[string]string
	Replies map[string][]map[string]any
	Fail    map[string]string
	// Channels are the workspace's conversations (conversations.list, filtered by its types,
	// exclude_archived and cursor; conversations.info answers a listed one as it is); History a
	// channel's messages, newest first (conversations.history, honouring oldest and limit);
	// Members a channel's member ids (conversations.members; a channel not listed is
	// channel_not_found).
	Channels []map[string]any
	History  map[string][]map[string]any
	Members  map[string][]string
	// RateLimit makes the next call of a method answer 429 with Retry-After 1.
	RateLimit map[string]bool
	// SlowUsers makes users.info for a user answer after a delay (set before the calls).
	SlowUsers map[string]time.Duration
	// Slow makes a method answer after a delay (or when the caller gives up).
	Slow map[string]time.Duration
	// DMUsers are the people of DM channels (conversations.info); a channel not listed is the DM
	// conversations.open gives the user (D0<id> for U<id>).
	DMUsers map[string]string
	// Profiles are users.info fields laid over a user's answer (team_id, is_restricted,
	// is_ultra_restricted, is_stranger, is_bot, …). A user not listed is a full member of Team.
	Profiles map[string]map[string]any
	// Files are the files the fake serves at /files/<id> (FileURL), to a request with the bot
	// token; downloads counts them.
	Files     map[string]FileBody
	downloads []string
	// ackAt is when each envelope was acked; responses the answers posted to response_urls
	ackAt     map[string]time.Time
	responses []Response
	// respondFail makes every response_url answer this HTTP status (0: 200; SetRespondFail).
	respondFail int
}

// Response is one answer a command's response_url received, and when.
type Response struct {
	ID, ResponseType, Text string
	ReplaceOriginal        bool
	Auth                   string
	At                     time.Time
}

// FileBody is a file the fake serves: its bytes and the Content-Type it answers with (Redirect,
// when set, answers with a redirect there instead).
type FileBody struct {
	Body        []byte
	ContentType string
	Redirect    string
}

// FileURL is where the fake serves a file (url_private_download).
func (s *Server) FileURL(id string) string { return s.URL + "/files/" + id }

// Downloads are the files fetched so far (ids), with the bot token.
func (s *Server) Downloads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.downloads...)
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/files/")
	if r.Header.Get("Authorization") != "Bearer "+BotToken {
		// as Slack does without the token: its sign-in page
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>sign in</html>"))
		return
	}
	s.mu.Lock()
	f, ok := s.Files[id]
	if ok {
		s.downloads = append(s.downloads, id)
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if f.Redirect != "" {
		http.Redirect(w, r, f.Redirect, http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", f.ContentType)
	_, _ = w.Write(f.Body)
}

// New starts a fake Slack; Close stops it (registered with t.Cleanup).
func New(t *testing.T) *Server {
	s := &Server{t: t, Names: map[string]string{}, Replies: map[string][]map[string]any{}, Fail: map[string]string{}, History: map[string][]map[string]any{}, Members: map[string][]string{},
		RateLimit: map[string]bool{}, SlowUsers: map[string]time.Duration{}, DMUsers: map[string]string{}, Profiles: map[string]map[string]any{}, Slow: map[string]time.Duration{}, connCh: make(chan struct{}, 16), nextTS: 1,
		Files: map[string]FileBody{}, ackAt: map[string]time.Time{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", s.api)
	mux.HandleFunc("/link", s.link)
	mux.HandleFunc("/files/", s.file)
	mux.HandleFunc("/upload/", s.upload)
	mux.HandleFunc("/respond/", s.respond)
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
	slow := s.Slow[method]
	s.mu.Unlock()
	if slow > 0 {
		select {
		case <-time.After(slow):
		case <-r.Context().Done():
			return
		}
	}
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
		reply(map[string]any{"ok": true, "user_id": BotUser, "bot_id": BotID, "team_id": Team, "user": "agent", "url": WorkspaceURL})
	case "apps.connections.open":
		s.mu.Lock()
		s.opens++
		n := s.opens
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "url": fmt.Sprintf("ws%s/link?n=%d", strings.TrimPrefix(s.URL, "http"), n)})
	case "files.getUploadURLExternal":
		s.mu.Lock()
		id := fmt.Sprintf("F0UP%06d", len(s.uploaded)+len(s.shares)*10+1)
		if s.uploaded == nil {
			s.uploaded = map[string][]byte{}
		}
		s.uploaded[id] = nil
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "upload_url": s.URL + "/upload/" + id, "file_id": id})
	case "files.completeUploadExternal":
		var files []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		}
		_ = json.Unmarshal([]byte(params["files"]), &files)
		sh := Share{Channel: params["channel_id"], Thread: params["thread_ts"], Comment: params["initial_comment"], Files: map[string][]byte{}}
		s.mu.Lock()
		for _, f := range files {
			sh.Files[f.Title] = s.uploaded[f.ID]
		}
		s.shares = append(s.shares, sh)
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "files": files})
	case "chat.postMessage":
		s.mu.Lock()
		ts := fmt.Sprintf("1800000000.%06d", s.nextTS)
		s.nextTS++
		s.posts = append(s.posts, Post{Channel: params["channel"], Thread: params["thread_ts"], Text: params["text"], TS: ts, Blocks: params["blocks"]})
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "channel": params["channel"], "ts": ts})
	case "users.info":
		s.mu.Lock()
		name, slow := s.Names[params["user"]], s.SlowUsers[params["user"]]
		user := map[string]any{"id": params["user"], "team_id": Team, "name": strings.ToLower(name), "profile": map[string]any{"display_name": name}}
		for k, v := range s.Profiles[params["user"]] {
			user[k] = v
		}
		s.mu.Unlock()
		if slow > 0 {
			select {
			case <-time.After(slow):
			case <-r.Context().Done():
			}
		}
		reply(map[string]any{"ok": true, "user": user})
	case "conversations.replies":
		s.mu.Lock()
		msgs := s.Replies[params["channel"]+":"+params["ts"]]
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "messages": msgs})
	case "conversations.list":
		reply(s.list(params))
	case "conversations.history":
		reply(s.history(params))
	case "conversations.members":
		s.mu.Lock()
		members, ok := s.Members[params["channel"]]
		s.mu.Unlock()
		if !ok {
			reply(map[string]any{"ok": false, "error": "channel_not_found"})
			return
		}
		reply(map[string]any{"ok": true, "members": members, "response_metadata": map[string]any{"next_cursor": ""}})
	case "conversations.info":
		ch := params["channel"]
		s.mu.Lock()
		user := s.DMUsers[ch]
		listed := s.channel(ch)
		s.mu.Unlock()
		if listed != nil {
			reply(map[string]any{"ok": true, "channel": listed})
			return
		}
		if !strings.HasPrefix(ch, "D") {
			reply(map[string]any{"ok": true, "channel": map[string]any{"id": ch, "is_channel": true}})
			return
		}
		if user == "" {
			user = "U" + strings.TrimPrefix(ch, "D0")
		}
		reply(map[string]any{"ok": true, "channel": map[string]any{"id": ch, "is_im": true, "user": user}})
	case "conversations.open":
		reply(map[string]any{"ok": true, "channel": map[string]any{"id": "D0" + strings.TrimPrefix(params["users"], "U")}})
	case "chat.update":
		s.mu.Lock()
		s.updates = append(s.updates, Update{Channel: params["channel"], TS: params["ts"], Text: params["text"], Blocks: params["blocks"]})
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "channel": params["channel"], "ts": params["ts"]})
	case "chat.postEphemeral":
		s.mu.Lock()
		s.ephemerals = append(s.ephemerals, Ephemeral{Channel: params["channel"], User: params["user"], Text: params["text"]})
		s.mu.Unlock()
		reply(map[string]any{"ok": true, "message_ts": "1.0"})
	case "reactions.add", "reactions.remove", "assistant.threads.setStatus":
		reply(map[string]any{"ok": true})
	default:
		reply(map[string]any{"ok": false, "error": "unknown_method"})
	}
}

// channel is the Channels entry with id (nil: none); the caller holds s.mu.
func (s *Server) channel(id string) map[string]any {
	for _, c := range s.Channels {
		if c["id"] == id {
			return c
		}
	}
	return nil
}

// kind is a Channels entry's conversations.list type.
func kind(c map[string]any) string {
	switch {
	case c["is_im"] == true:
		return "im"
	case c["is_mpim"] == true:
		return "mpim"
	case c["is_private"] == true:
		return "private_channel"
	}
	return "public_channel"
}

// list is conversations.list over Channels: the types asked for (public_channel by default),
// archived ones left out when exclude_archived, a page of limit from cursor (an index).
func (s *Server) list(params map[string]string) map[string]any {
	types := strings.Split(params["types"], ",")
	if params["types"] == "" {
		types = []string{"public_channel"}
	}
	limit, start := 100, 0
	fmt.Sscan(params["limit"], &limit)
	fmt.Sscan(params["cursor"], &start)
	s.mu.Lock()
	defer s.mu.Unlock()
	match := []map[string]any{}
	for _, c := range s.Channels {
		if !slices.Contains(types, kind(c)) || params["exclude_archived"] == "true" && c["is_archived"] == true {
			continue
		}
		match = append(match, c)
	}
	start = min(start, len(match))
	end := min(start+limit, len(match))
	next := ""
	if end < len(match) {
		next = fmt.Sprint(end)
	}
	return map[string]any{"ok": true, "channels": match[start:end], "response_metadata": map[string]any{"next_cursor": next}}
}

// history is conversations.history over History: newest first, those after oldest, at most
// limit, has_more when more were left out.
func (s *Server) history(params map[string]string) map[string]any {
	limit := 100
	fmt.Sscan(params["limit"], &limit)
	oldest := params["oldest"]
	s.mu.Lock()
	msgs := s.History[params["channel"]]
	s.mu.Unlock()
	out := []map[string]any{}
	more := false
	for _, m := range msgs {
		if ts, _ := m["ts"].(string); oldest != "" && ts <= oldest {
			continue
		}
		if len(out) == limit {
			more = true
			break
		}
		out = append(out, m)
	}
	return map[string]any{"ok": true, "messages": out, "has_more": more}
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
			s.ackAt[ack.EnvelopeID] = time.Now()
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
	// UserTeam, when set, is the author's own team (a Slack Connect user's differs from Team).
	UserTeam string
	// Blocks, when set, is the event's blocks (rich text).
	Blocks any
	// Files, when set, are the files the message carries (Slack's file objects: id, name,
	// mimetype, filetype, size, mode, subtype, url_private_download, …); its subtype is then
	// file_share unless set.
	Files []map[string]any
}

// Send sends an events_api envelope with the event, as envelope id.
func (s *Server) Send(id string, e Event) {
	s.t.Helper()
	if e.Type == "" {
		e.Type = "message"
	}
	ev := map[string]any{"type": e.Type, "channel": e.Channel, "user": e.User, "text": e.Text, "ts": e.TS, "team": Team}
	for k, v := range map[string]string{"subtype": e.Subtype, "channel_type": e.ChannelType, "bot_id": e.BotID, "thread_ts": e.ThreadTS, "user_team": e.UserTeam} {
		if v != "" {
			ev[k] = v
		}
	}
	if e.Blocks != nil {
		ev["blocks"] = e.Blocks
	}
	if len(e.Files) > 0 {
		ev["files"] = e.Files
		if e.Subtype == "" {
			ev["subtype"] = "file_share"
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

// Click sends an interactive envelope as Slack does when user clicks a button of a message the
// bot posted (channel, ts): a block_actions payload with the button's action id and value.
func (s *Server) Click(id, user, channel, ts, actionID, value string) {
	s.t.Helper()
	payload := map[string]any{"type": "block_actions", "team": map[string]any{"id": Team}, "user": map[string]any{"id": user, "team_id": Team},
		"container": map[string]any{"type": "message", "message_ts": ts, "channel_id": channel, "is_ephemeral": false},
		"channel":   map[string]any{"id": channel}, "trigger_id": "1.2.3",
		"actions": []map[string]any{{"type": "button", "action_id": actionID, "block_id": "needletail_approval", "value": value, "action_ts": "1800000000.000001"}}}
	env := map[string]any{"envelope_id": id, "type": "interactive", "accepts_response_payload": false, "payload": payload}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		s.t.Fatal("slacktest: no connection")
	}
	s.write(conn, env)
}

// Updates are the chat.update calls so far.
func (s *Server) Updates() []Update {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Update{}, s.updates...)
}

// Ephemerals are the chat.postEphemeral calls so far.
func (s *Server) Ephemerals() []Ephemeral {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Ephemeral{}, s.ephemerals...)
}

// WaitUpdates waits until at least n chat.update calls came and returns them.
func (s *Server) WaitUpdates(n int, timeout time.Duration) []Update {
	s.t.Helper()
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if u := s.Updates(); len(u) >= n {
			return u
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Fatalf("slacktest: %d updates after %s, want %d", len(s.Updates()), timeout, n)
	return nil
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
// Share is a files.completeUploadExternal: where the files went, their comment, and each file's
// bytes by name.
type Share struct {
	Channel, Thread, Comment string
	Files                    map[string][]byte
}

// Shares are the files posted so far.
func (s *Server) Shares() []Share {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Share{}, s.shares...)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/upload/")
	b, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.uploaded[id]; !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.uploaded[id] = b
}

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

// ResponseURL is a command's response_url on the fake (answers to it are kept as Responses).
func (s *Server) ResponseURL(id string) string { return s.URL + "/respond/" + id }

func (s *Server) respond(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ResponseType    string `json:"response_type"`
		Text            string `json:"text"`
		ReplaceOriginal bool   `json:"replace_original"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	fail := s.respondFail
	s.responses = append(s.responses, Response{ID: strings.TrimPrefix(r.URL.Path, "/respond/"), ResponseType: body.ResponseType, Text: body.Text,
		ReplaceOriginal: body.ReplaceOriginal, Auth: r.Header.Get("Authorization"), At: time.Now()})
	s.mu.Unlock()
	if fail != 0 {
		w.WriteHeader(fail)
		return
	}
	_, _ = w.Write([]byte("ok"))
}

// SetRespondFail makes every response_url answer status (0: 200 again).
func (s *Server) SetRespondFail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respondFail = status
}

// Responses are the answers posted to response_urls so far.
func (s *Server) Responses() []Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Response{}, s.responses...)
}

// WaitResponses waits until n answers reached response_urls, and returns them.
func (s *Server) WaitResponses(n int, timeout time.Duration) []Response {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if r := s.Responses(); len(r) >= n {
			return r
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("slacktest: %d command answers after %s, want %d", len(s.Responses()), timeout, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// AckTime is when the envelope id was acked (zero: not acked).
func (s *Server) AckTime(id string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ackAt[id]
}

// Slash sends a slash_commands envelope as Slack does when user runs command (with text) in
// channel; its response_url is the fake's (ResponseURL(id)). Extra fields (thread_ts) go into
// the payload as given.
func (s *Server) Slash(id, command, text, user, channel string, extra map[string]string) {
	s.t.Helper()
	payload := map[string]any{"token": "deprecated", "team_id": Team, "team_domain": "acme-test", "channel_id": channel, "channel_name": "directmessage",
		"user_id": user, "user_name": "someone", "command": command, "text": text, "api_app_id": "A0APP00001", "is_enterprise_install": "false",
		"response_url": s.ResponseURL(id), "trigger_id": "1.2.3"}
	for k, v := range extra {
		payload[k] = v
	}
	env := map[string]any{"envelope_id": id, "type": "slash_commands", "accepts_response_payload": true, "payload": payload}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		s.t.Fatal("slacktest: no connection")
	}
	s.write(conn, env)
}
