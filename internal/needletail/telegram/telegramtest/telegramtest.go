// Package telegramtest is a fake Telegram Bot API for tests, on an httptest server (a loopback
// URL): getMe, getUpdates (long polling over the updates a test pushes), sendMessage,
// editMessageText, sendChatAction, setMyCommands, getFile and file downloads. Nothing in it talks
// to Telegram. Ids are made up.
package telegramtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fake's bot: its token (not a Telegram token) and identity.
const (
	Token       = "123456:fake-telegram-token-for-tests"
	BotID       = int64(123456)
	BotUsername = "acme_test_bot"
)

// Sent is one sendMessage (or editMessageText: Edit).
type Sent struct {
	Chat      int64
	Thread    int
	ReplyTo   int
	Text      string
	ParseMode string
	ID        int
	Edit      bool
}

// Call is one Bot API call: its method and parameters.
type Call struct {
	Method string
	Params map[string]any
}

// File is a file the fake serves (getFile, then /file/bot<token>/<path>).
type File struct {
	Body []byte
	Size int64 // reported by getFile (0: len(Body))
}

// Server is the fake Bot API.
type Server struct {
	*httptest.Server
	t *testing.T

	mu        sync.Mutex
	updates   []map[string]any
	nextID    int64
	confirmed int64
	sent      []Sent
	calls     []Call
	nextMsg   int
	files     map[string]File
	downloads []string
	wake      chan struct{}
	// rejectMarkdown makes sendMessage refuse MarkdownV2 (as for text Telegram cannot parse).
	rejectMarkdown bool
}

// New starts a fake Bot API; it stops with the test.
func New(t *testing.T) *Server {
	s := &Server{t: t, nextID: 1000, nextMsg: 5000, files: map[string]File{}, wake: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/bot"+Token+"/", s.api)
	mux.HandleFunc("/file/bot"+Token+"/", s.file)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error_code":401,"description":"Unauthorized"}`))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/bot"+Token+"/")
	params := map[string]any{}
	_ = json.NewDecoder(r.Body).Decode(&params)
	s.mu.Lock()
	s.calls = append(s.calls, Call{Method: method, Params: params})
	s.mu.Unlock()
	ok := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}
	fail := func(code int, desc string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": code, "description": desc})
	}
	num := func(k string) int64 {
		f, _ := params[k].(float64)
		return int64(f)
	}
	switch method {
	case "getMe":
		ok(map[string]any{"id": BotID, "is_bot": true, "first_name": "Acme", "username": BotUsername})
	case "getUpdates":
		offset, timeout := num("offset"), num("timeout")
		deadline := time.Now().Add(time.Duration(timeout) * time.Second)
		for {
			s.mu.Lock()
			if offset > s.confirmed {
				s.confirmed = offset
			}
			out := []map[string]any{}
			for _, u := range s.updates {
				if id, _ := u["update_id"].(int64); id >= offset {
					out = append(out, u)
				}
			}
			s.mu.Unlock()
			if len(out) > 0 || timeout == 0 || time.Now().After(deadline) {
				ok(out)
				return
			}
			select {
			case <-s.wake:
			case <-r.Context().Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	case "sendMessage", "editMessageText":
		s.mu.Lock()
		reject := s.rejectMarkdown
		s.mu.Unlock()
		if params["parse_mode"] == "MarkdownV2" && reject {
			fail(400, "Bad Request: can't parse entities: character '!' is reserved")
			return
		}
		text, _ := params["text"].(string)
		if len([]rune(text)) > 4096 {
			fail(400, "Bad Request: message is too long")
			return
		}
		m := Sent{Chat: num("chat_id"), Thread: int(num("message_thread_id")), Text: text, Edit: method == "editMessageText"}
		m.ParseMode, _ = params["parse_mode"].(string)
		if rp, _ := params["reply_parameters"].(map[string]any); rp != nil {
			f, _ := rp["message_id"].(float64)
			m.ReplyTo = int(f)
		}
		s.mu.Lock()
		if m.Edit {
			m.ID = int(num("message_id"))
		} else {
			s.nextMsg++
			m.ID = s.nextMsg
		}
		s.sent = append(s.sent, m)
		s.mu.Unlock()
		ok(map[string]any{"message_id": m.ID, "chat": map[string]any{"id": m.Chat}, "date": time.Now().Unix(), "text": text})
	case "sendChatAction", "setMyCommands":
		ok(true)
	case "getFile":
		id, _ := params["file_id"].(string)
		s.mu.Lock()
		f, found := s.files[id]
		s.mu.Unlock()
		if !found {
			fail(400, "Bad Request: invalid file_id")
			return
		}
		size := f.Size
		if size == 0 {
			size = int64(len(f.Body))
		}
		ok(map[string]any{"file_id": id, "file_unique_id": "u" + id, "file_size": size, "file_path": "files/" + id})
	default:
		fail(404, "Not Found: method not found")
	}
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/file/bot"+Token+"/files/")
	s.mu.Lock()
	f, ok := s.files[id]
	if ok {
		s.downloads = append(s.downloads, id)
	}
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(f.Body)
}

// API is the Bot API base URL to give the gateway.
func (s *Server) API() string { return s.URL }

// Push queues a message update (as Telegram would for the bot) and returns its update id.
func (s *Server) Push(message map[string]any) int64 {
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.updates = append(s.updates, map[string]any{"update_id": id, "message": message})
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return id
}

// Message is a message update's message from user (id, first name) in chat (type private or
// group); extra fields are laid over it.
func Message(id int, chat int64, chatType string, user int64, name, text string, extra map[string]any) map[string]any {
	m := map[string]any{"message_id": id, "date": time.Now().Unix(), "chat": map[string]any{"id": chat, "type": chatType},
		"from": map[string]any{"id": user, "is_bot": false, "first_name": name}}
	if text != "" {
		m["text"] = text
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// Mention is a group message that @mentions the bot.
func Mention(id int, chat, user int64, name, text string) map[string]any {
	full := "@" + BotUsername + " " + text
	return Message(id, chat, "group", user, name, full, map[string]any{"entities": []map[string]any{{"type": "mention", "offset": 0, "length": len(BotUsername) + 1}}})
}

// SetFile makes the fake serve a file under id.
func (s *Server) SetFile(id string, f File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files[id] = f
}

// RejectMarkdown makes sendMessage refuse MarkdownV2 text (true) or take it again.
func (s *Server) RejectMarkdown(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectMarkdown = on
}

// Sent are the messages sent (and edited) so far.
func (s *Server) Sent() []Sent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sent{}, s.sent...)
}

// WaitSent waits until n messages were sent, and returns them.
func (s *Server) WaitSent(n int, timeout time.Duration) []Sent {
	s.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if m := s.Sent(); len(m) >= n {
			return m
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("telegramtest: %d messages sent after %s, want %d: %+v", len(s.Sent()), timeout, n, s.Sent())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Calls are the calls of a method so far ("" for all).
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

// Downloads are the files downloaded so far (ids).
func (s *Server) Downloads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.downloads...)
}

// Confirmed is the highest offset the gateway confirmed (updates before it are taken).
func (s *Server) Confirmed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.confirmed
}

// String names the fake.
func (s *Server) String() string { return fmt.Sprintf("telegramtest at %s", s.URL) }
