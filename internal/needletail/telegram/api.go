// Package telegram is the runtime's Telegram gateway (docs/needletail.md §4.14): the Bot API with
// long polling (getUpdates; no webhook, so no public URL), the bot token only ever sent to
// api.telegram.org, a conversation per chat, the agent's handlers and the messaging policy as on
// Slack. Behaviour follows Hermes' Telegram platform (the hermes-agent image's
// plugins/platforms/telegram) where it means something here.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPI is Telegram's Bot API.
const DefaultAPI = "https://api.telegram.org"

// MaxFileBytes is the largest file the Bot API lets a bot download (getFile).
const MaxFileBytes = 20 << 20

// CheckBaseURL accepts Telegram's own API or, for tests, an http(s) URL on the loopback interface:
// an override can never send the token to another host.
func CheckBaseURL(raw string) error {
	if raw == "" || raw == DefaultAPI {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("telegram API URL is not an http URL")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("telegram API URL: an override must be a loopback address (tests only)")
	}
	return nil
}

// APIError is a Bot API answer with ok false: its error_code and description (which never hold
// the token).
type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, oneLine(e.Description, 200))
}

// DeliveryPermanent: a chat that is gone, a bot that was blocked or kicked, a refused token are
// not worth retrying (the scheduler's outbox).
func (e *APIError) DeliveryPermanent() bool {
	return e != nil && (e.Code == 400 && strings.Contains(strings.ToLower(e.Description), "chat not found") || e.Code == 401 || e.Code == 403)
}

// DeliveryClass is the scheduler's coarse class of a delivery failure.
func (e *APIError) DeliveryClass() string {
	switch {
	case e == nil:
		return ""
	case e.Code == 401:
		return "auth"
	case e.Code == 403 || e.Code == 400 && strings.Contains(strings.ToLower(e.Description), "chat not found"):
		return "not_in_channel"
	}
	return "transient"
}

// Bot calls the Bot API with the bot's token.
type Bot struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// MaxRetryAfter is the longest retry_after a rate-limited call waits for (default 30 s).
	MaxRetryAfter time.Duration
}

func (b *Bot) base() string {
	if b.BaseURL == "" {
		return DefaultAPI
	}
	return strings.TrimRight(b.BaseURL, "/")
}

func (b *Bot) client(timeout time.Duration) *http.Client {
	c := &http.Client{}
	if b.HTTP != nil {
		cc := *b.HTTP
		c = &cc
	}
	c.Timeout = timeout
	// the token is in the path: a redirect would carry it elsewhere
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirected: refused") }
	return c
}

// call POSTs params as JSON to method and decodes the result into out. Errors never carry the URL
// (which holds the token).
func (b *Bot) call(ctx context.Context, method string, params any, out any, timeout time.Duration) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	maxWait := b.MaxRetryAfter
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base()+"/bot"+b.Token+"/"+method, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("telegram %s: bad request", method)
		}
		req.Header.Set("Content-Type", "application/json")
		res, err := b.client(timeout).Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("telegram %s: %w", method, ctx.Err())
			}
			var ue *url.Error
			if errors.As(err, &ue) {
				return fmt.Errorf("telegram %s: %v", method, ue.Err) // the error without the URL
			}
			return fmt.Errorf("telegram %s: request failed", method)
		}
		raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		res.Body.Close()
		if err != nil {
			return fmt.Errorf("telegram %s: reading the answer failed", method)
		}
		var env struct {
			OK          bool            `json:"ok"`
			Result      json.RawMessage `json:"result"`
			ErrorCode   int             `json:"error_code"`
			Description string          `json:"description"`
			Parameters  struct {
				RetryAfter int `json:"retry_after"`
			} `json:"parameters"`
		}
		if json.Unmarshal(raw, &env) != nil {
			return fmt.Errorf("telegram %s: HTTP %d", method, res.StatusCode)
		}
		if !env.OK {
			e := &APIError{Method: method, Code: env.ErrorCode, Description: env.Description, RetryAfter: env.Parameters.RetryAfter}
			if e.Code == 0 {
				e.Code = res.StatusCode
			}
			if e.Code == 429 && attempt < 3 {
				wait := time.Duration(max(1, e.RetryAfter)) * time.Second
				if wait > maxWait {
					return e
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("telegram %s: %w", method, ctx.Err())
				case <-time.After(wait):
				}
				continue
			}
			return e
		}
		if out != nil {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return fmt.Errorf("telegram %s: unexpected answer", method)
			}
		}
		return nil
	}
}

// User is a Telegram user (or bot).
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

// Name is how the input names a user: their name, else their username, else their id.
func (u User) Name() string {
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	switch {
	case n != "":
		return n
	case u.Username != "":
		return u.Username
	}
	return strconv.FormatInt(u.ID, 10)
}

// Chat is a chat: private (a person's), group, supergroup or channel.
type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
	IsForum  bool   `json:"is_forum"`
}

// Entity is a piece of a message's text with a meaning (a mention, a command); Offset and Length
// count UTF-16 code units.
type Entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	User   *User  `json:"user"`
}

// PhotoSize is one size of a photo.
type PhotoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size"`
}

// Media is a voice note, an audio file or a document.
type Media struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
	Duration     int    `json:"duration"`
}

// Message is a message (the fields used here).
type Message struct {
	MessageID       int         `json:"message_id"`
	MessageThreadID int         `json:"message_thread_id"`
	IsTopicMessage  bool        `json:"is_topic_message"`
	From            *User       `json:"from"`
	Chat            Chat        `json:"chat"`
	Date            int64       `json:"date"`
	Text            string      `json:"text"`
	Caption         string      `json:"caption"`
	Entities        []Entity    `json:"entities"`
	CaptionEntities []Entity    `json:"caption_entities"`
	ReplyTo         *Message    `json:"reply_to_message"`
	Photo           []PhotoSize `json:"photo"`
	Voice           *Media      `json:"voice"`
	Audio           *Media      `json:"audio"`
	Document        *Media      `json:"document"`
}

// Update is one getUpdates item (only messages are asked for).
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// GetMe is the bot's own user.
func (b *Bot) GetMe(ctx context.Context) (User, error) {
	var u User
	err := b.call(ctx, "getMe", map[string]any{}, &u, 30*time.Second)
	return u, err
}

// GetUpdates long-polls for messages after offset (waiting up to timeout seconds); asking with an
// offset confirms every update before it.
func (b *Bot) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var out []Update
	err := b.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": timeout, "limit": 100, "allowed_updates": []string{"message"}},
		&out, time.Duration(timeout+15)*time.Second)
	return out, err
}

// Send is one sendMessage: text in chat (a forum topic's thread, and a reply to a message, when
// set), with parse_mode ("" for plain text).
type Send struct {
	Chat      int64
	Thread    int
	ReplyTo   int
	Text      string
	ParseMode string
}

// SendMessage posts a message and returns its id.
func (b *Bot) SendMessage(ctx context.Context, s Send) (int, error) {
	p := map[string]any{"chat_id": s.Chat, "text": s.Text, "link_preview_options": map[string]any{"is_disabled": true}}
	if s.ParseMode != "" {
		p["parse_mode"] = s.ParseMode
	}
	if s.Thread != 0 {
		p["message_thread_id"] = s.Thread
	}
	if s.ReplyTo != 0 {
		p["reply_parameters"] = map[string]any{"message_id": s.ReplyTo, "allow_sending_without_reply": true}
	}
	var m Message
	err := b.call(ctx, "sendMessage", p, &m, 30*time.Second)
	return m.MessageID, err
}

// EditMessage replaces a message's text (plain).
func (b *Bot) EditMessage(ctx context.Context, chat int64, id int, text string) error {
	return b.call(ctx, "editMessageText", map[string]any{"chat_id": chat, "message_id": id, "text": text}, nil, 30*time.Second)
}

// Typing shows "typing…" in a chat for a few seconds.
func (b *Bot) Typing(ctx context.Context, chat int64, thread int) error {
	p := map[string]any{"chat_id": chat, "action": "typing"}
	if thread != 0 {
		p["message_thread_id"] = thread
	}
	return b.call(ctx, "sendChatAction", p, nil, 10*time.Second)
}

// Command is one entry of the bot's command menu.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// SetCommands sets the bot's command menu (setMyCommands).
func (b *Bot) SetCommands(ctx context.Context, cmds []Command) error {
	return b.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil, 30*time.Second)
}

// File is getFile's answer: where the file is (its path under the file URL).
type File struct {
	FileID   string `json:"file_id"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"`
}

// Download is a file's bytes (getFile, then the file URL, which carries the token: the same host,
// no redirect), at most max.
func (b *Bot) Download(ctx context.Context, fileID string, max int64) ([]byte, error) {
	var f File
	if err := b.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f, 30*time.Second); err != nil {
		return nil, err
	}
	if f.FileSize > max {
		return nil, fmt.Errorf("over %d MB", max>>20)
	}
	if f.FilePath == "" || strings.Contains(f.FilePath, "..") || strings.HasPrefix(f.FilePath, "/") || strings.ContainsAny(f.FilePath, "?#\\") {
		return nil, errors.New("telegram gave no usable file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base()+"/file/bot"+b.Token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, errors.New("bad file path")
	}
	res, err := b.client(time.Minute).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("the download timed out")
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			return nil, fmt.Errorf("download failed: %v", ue.Err)
		}
		return nil, errors.New("download failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: HTTP %d", res.StatusCode)
	}
	if res.ContentLength > max {
		return nil, fmt.Errorf("over %d MB", max>>20)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil {
		return nil, errors.New("download failed while reading")
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("over %d MB", max>>20)
	}
	return body, nil
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
