// Package slack is the runtime's Slack gateway (docs/needletail.md §4.9): a Socket Mode client
// (socket.go) that hears the agent's app, the Web API calls it answers with (this file), the
// rules for whom and what it answers (gateway.go), Slack's text format (format.go), and the
// scheduler's deliverer and the send_message tool (deliver.go). Behaviour follows Hermes' Slack
// platform (the hermes-agent image's plugins/platforms/slack) where it means something here.
package slack

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

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// DefaultAPI is Slack's Web API.
const DefaultAPI = "https://slack.com/api"

// APIError is a Web API answer with ok false (Code is Slack's "error", e.g. channel_not_found).
type APIError struct {
	Method, Code string
}

func (e *APIError) Error() string { return fmt.Sprintf("slack %s: %s", e.Method, e.Code) }

// DeliveryPermanent classifies destination/auth failures for the scheduler without
// exposing provider-specific codes through its generic delivery interface. Unknown
// errors remain retryable; errors.As also finds this method through wrappers.
func (e *APIError) DeliveryPermanent() bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case "channel_not_found", "not_in_channel", "is_archived", "missing_scope", "invalid_auth", "account_inactive":
		return true
	}
	return false
}

// DeliveryClass is the scheduler's coarse class of a delivery failure (sched.errorClass).
func (e *APIError) DeliveryClass() string {
	if e == nil {
		return ""
	}
	switch e.Code {
	case "channel_not_found", "not_in_channel", "is_archived":
		return "not_in_channel"
	case "missing_scope", "invalid_auth", "account_inactive", "not_authed":
		return "auth"
	}
	return "transient"
}

// Web calls Slack's Web API with the bot token (BotToken), or the app token for
// apps.connections.open (AppToken).
type Web struct {
	BaseURL            string
	BotToken, AppToken string
	HTTP               *http.Client
	// MaxRetryAfter is the longest Retry-After a rate-limited call waits for before it fails
	// (default 30 s); at most three waits per call.
	MaxRetryAfter time.Duration
	sleep         func(context.Context, time.Duration) error
}

// CheckBaseURL accepts Slack's own API or, for tests, an http(s) URL on the loopback interface:
// an override can never send the tokens to another host.
func CheckBaseURL(raw string) error {
	if raw == "" || raw == DefaultAPI {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("slack API URL %q is not an http URL", raw)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("slack API URL %q: an override must be a loopback address (tests only)", raw)
	}
	return nil
}

func (w *Web) base() string {
	if w.BaseURL == "" {
		return DefaultAPI
	}
	return strings.TrimRight(w.BaseURL, "/")
}

// call POSTs a form to method and decodes the answer into out; an answer with ok false is an
// *APIError. A 429 is retried after its Retry-After (bounded).
func (w *Web) call(ctx context.Context, method, token string, params url.Values, out any) error {
	hc := w.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	maxWait := w.MaxRetryAfter
	if maxWait <= 0 {
		maxWait = 30 * time.Second
	}
	sleep := w.sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.base()+"/"+method, strings.NewReader(params.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
		res.Body.Close()
		if err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if res.StatusCode == http.StatusTooManyRequests {
			wait, _ := strconv.Atoi(res.Header.Get("Retry-After"))
			d := time.Duration(max(wait, 1)) * time.Second
			if attempt >= 3 || d > maxWait {
				return &APIError{Method: method, Code: "ratelimited"}
			}
			if err := sleep(ctx, d); err != nil {
				return err
			}
			continue
		}
		if res.StatusCode >= 300 {
			return fmt.Errorf("slack %s: HTTP %d", method, res.StatusCode)
		}
		var head struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(body, &head); err != nil {
			return fmt.Errorf("slack %s: %w", method, err)
		}
		if !head.OK {
			return &APIError{Method: method, Code: orDefault(head.Error, "unknown_error")}
		}
		if out != nil {
			return json.Unmarshal(body, out)
		}
		return nil
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Identity is auth.test's answer: the bot's own user and bot ids.
type Identity struct {
	UserID string `json:"user_id"`
	BotID  string `json:"bot_id"`
	TeamID string `json:"team_id"`
	User   string `json:"user"`
	// URL is the workspace's address (https://<workspace>.slack.com/), for permalinks.
	URL string `json:"url"`
}

// AuthTest is who the bot token is.
func (w *Web) AuthTest(ctx context.Context) (Identity, error) {
	var id Identity
	err := w.call(ctx, "auth.test", w.BotToken, url.Values{}, &id)
	return id, err
}

// ConnectionsOpen is a Socket Mode websocket URL (the app token's).
func (w *Web) ConnectionsOpen(ctx context.Context) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	if err := w.call(ctx, "apps.connections.open", w.AppToken, url.Values{}, &out); err != nil {
		return "", err
	}
	if out.URL == "" {
		return "", errors.New("slack apps.connections.open: no url")
	}
	return out.URL, nil
}

// PostMessage posts text (Slack mrkdwn) to channel, in thread when it is set; it returns the
// message's ts.
func (w *Web) PostMessage(ctx context.Context, channel, thread, text string) (string, error) {
	p := url.Values{"channel": {channel}, "text": {text}, "unfurl_links": {"false"}, "unfurl_media": {"false"}}
	if thread != "" {
		p.Set("thread_ts", thread)
	}
	var out struct {
		TS string `json:"ts"`
	}
	err := w.call(ctx, "chat.postMessage", w.BotToken, p, &out)
	return out.TS, err
}

// Update replaces a message's text (unused until replies stream: Hermes' edit fallback).
func (w *Web) Update(ctx context.Context, channel, ts, text string) error {
	return w.call(ctx, "chat.update", w.BotToken, url.Values{"channel": {channel}, "ts": {ts}, "text": {text}}, nil)
}

// PostBlocks posts a Block Kit message (blocks, a JSON array) with text as its notification and
// fallback; it returns the message's ts. Never unfurled.
func (w *Web) PostBlocks(ctx context.Context, channel, thread, text string, blocks json.RawMessage) (string, error) {
	p := url.Values{"channel": {channel}, "text": {text}, "blocks": {string(blocks)}, "unfurl_links": {"false"}, "unfurl_media": {"false"}}
	if thread != "" {
		p.Set("thread_ts", thread)
	}
	var out struct {
		TS string `json:"ts"`
	}
	err := w.call(ctx, "chat.postMessage", w.BotToken, p, &out)
	return out.TS, err
}

// UpdateBlocks replaces a message's text and blocks (an approval card once decided).
func (w *Web) UpdateBlocks(ctx context.Context, channel, ts, text string, blocks json.RawMessage) error {
	return w.call(ctx, "chat.update", w.BotToken, url.Values{"channel": {channel}, "ts": {ts}, "text": {text}, "blocks": {string(blocks)}}, nil)
}

// Ephemeral shows text to one user in a channel (chat.postEphemeral): why their click did nothing.
func (w *Web) Ephemeral(ctx context.Context, channel, user, text string) error {
	return w.call(ctx, "chat.postEphemeral", w.BotToken, url.Values{"channel": {channel}, "user": {user}, "text": {text}}, nil)
}

// React adds (or with remove, removes) an emoji reaction to a message.
func (w *Web) React(ctx context.Context, channel, ts, name string, remove bool) error {
	method := "reactions.add"
	if remove {
		method = "reactions.remove"
	}
	return w.call(ctx, method, w.BotToken, url.Values{"channel": {channel}, "timestamp": {ts}, "name": {name}}, nil)
}

// SetStatus sets (status "" clears) the assistant thread status line ("is thinking...").
func (w *Web) SetStatus(ctx context.Context, channel, thread, status string) error {
	return w.call(ctx, "assistant.threads.setStatus", w.BotToken, url.Values{"channel_id": {channel}, "thread_ts": {thread}, "status": {status}}, nil)
}

// Message is a message as the Web API and the Events API give it (the fields used here).
type Message struct {
	Type        string          `json:"type"`
	Subtype     string          `json:"subtype"`
	Channel     string          `json:"channel"`
	ChannelType string          `json:"channel_type"`
	User        string          `json:"user"`
	BotID       string          `json:"bot_id"`
	BotProfile  json.RawMessage `json:"bot_profile"`
	Text        string          `json:"text"`
	TS          string          `json:"ts"`
	ThreadTS    string          `json:"thread_ts"`
	Team        string          `json:"team"`
	// UserTeam is the author's own team (Slack Connect: another workspace's), when Slack says.
	UserTeam string `json:"user_team"`
	// Blocks are the message's blocks (rich text names a mention as a user element).
	Blocks json.RawMessage `json:"blocks"`
	// Files are the files it carries (a file_share).
	Files []File `json:"files"`
	// ReplyCount is how many replies a thread's parent has (conversations.history).
	ReplyCount int `json:"reply_count"`
}

// Replies is a thread's messages, oldest first, at most limit (the root included).
func (w *Web) Replies(ctx context.Context, channel, thread string, limit int) ([]Message, error) {
	var out struct {
		Messages []Message `json:"messages"`
	}
	err := w.call(ctx, "conversations.replies", w.BotToken, url.Values{"channel": {channel}, "ts": {thread}, "limit": {strconv.Itoa(limit)}, "inclusive": {"true"}}, &out)
	return out.Messages, err
}

// History is a channel's messages after oldest (a ts; "" for no bound), newest first, at most
// limit, and whether Slack has more (conversations.history).
func (w *Web) History(ctx context.Context, channel, oldest string, limit int) ([]Message, bool, error) {
	var out struct {
		Messages []Message `json:"messages"`
		HasMore  bool      `json:"has_more"`
	}
	p := url.Values{"channel": {channel}, "limit": {strconv.Itoa(limit)}, "include_all_metadata": {"false"}}
	if oldest != "" {
		p.Set("oldest", oldest)
	}
	err := w.call(ctx, "conversations.history", w.BotToken, p, &out)
	return out.Messages, out.HasMore, err
}

// Members is a page of a channel's member ids, and the cursor of the next ("" at the end)
// (conversations.members).
func (w *Web) Members(ctx context.Context, channel, cursor string) ([]string, string, error) {
	var out struct {
		Members  []string `json:"members"`
		Metadata struct {
			Next string `json:"next_cursor"`
		} `json:"response_metadata"`
	}
	p := url.Values{"channel": {channel}, "limit": {"1000"}}
	if cursor != "" {
		p.Set("cursor", cursor)
	}
	err := w.call(ctx, "conversations.members", w.BotToken, p, &out)
	return out.Members, out.Metadata.Next, err
}

// User is users.info's answer (the fields used here).
type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	IsBot bool   `json:"is_bot"`
	// TeamID is the user's own workspace; IsRestricted (a multi-channel guest),
	// IsUltraRestricted (a single-channel guest) and IsStranger (an external user Slack Connect
	// shows) say they are not a full member of it.
	TeamID            string `json:"team_id"`
	IsRestricted      bool   `json:"is_restricted"`
	IsUltraRestricted bool   `json:"is_ultra_restricted"`
	IsStranger        bool   `json:"is_stranger"`
	Deleted           bool   `json:"deleted"`
	Profile           struct {
		DisplayName string `json:"display_name"`
		RealName    string `json:"real_name"`
	} `json:"profile"`
}

// DisplayName is the name Hermes shows: display name, else real name, else handle, else id.
func (u User) DisplayName() string {
	for _, n := range []string{u.Profile.DisplayName, u.Profile.RealName, u.Name} {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	return u.ID
}

// UserInfo is one user.
func (w *Web) UserInfo(ctx context.Context, id string) (User, error) {
	var out struct {
		User User `json:"user"`
	}
	err := w.call(ctx, "users.info", w.BotToken, url.Values{"user": {id}}, &out)
	return out.User, err
}

// DMUser is the person of a DM channel (conversations.info's user; "" for a channel that is not a
// DM).
func (w *Web) DMUser(ctx context.Context, channel string) (string, error) {
	var out struct {
		Channel struct {
			IsIM bool   `json:"is_im"`
			User string `json:"user"`
		} `json:"channel"`
	}
	if err := w.call(ctx, "conversations.info", w.BotToken, url.Values{"channel": {channel}}, &out); err != nil {
		return "", err
	}
	if !out.Channel.IsIM {
		return "", nil
	}
	return out.Channel.User, nil
}

// OpenDM is the DM channel with a user (conversations.open).
func (w *Web) OpenDM(ctx context.Context, user string) (string, error) {
	var out struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	err := w.call(ctx, "conversations.open", w.BotToken, url.Values{"users": {user}}, &out)
	return out.Channel.ID, err
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// MaxUpload is the most one file posted with a message may hold.
const MaxUpload = 100 << 20

// checkUploadURL accepts the upload URL Slack hands out (https on files.slack.com) or, while the
// API is a loopback test server, a loopback URL: the file's bytes never go anywhere else.
func (w *Web) checkUploadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("slack gave an upload URL that does not parse")
	}
	if u.Scheme == "https" && u.Hostname() == "files.slack.com" {
		return nil
	}
	if w.BaseURL != "" && w.BaseURL != DefaultAPI && CheckBaseURL(raw) == nil {
		return nil
	}
	return fmt.Errorf("slack gave an upload URL on %q, not files.slack.com: the file is not sent", u.Hostname())
}

// UploadFiles posts files to a channel (in a thread when thread is set) as one message with
// comment as its text: files.getUploadURLExternal for each, its bytes to the URL Slack gave, then
// files.completeUploadExternal sharing them all. It gives the files' ids.
func (w *Web) UploadFiles(ctx context.Context, channel, thread, comment string, files []messaging.File) ([]string, error) {
	hc := w.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Minute}
	}
	type done struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	var shared []done
	for _, f := range files {
		if len(f.Data) > MaxUpload {
			return nil, fmt.Errorf("%s is over %d MB", f.Name, MaxUpload>>20)
		}
		var slot struct {
			UploadURL string `json:"upload_url"`
			FileID    string `json:"file_id"`
		}
		p := url.Values{"filename": {f.Name}, "length": {strconv.Itoa(len(f.Data))}}
		if err := w.call(ctx, "files.getUploadURLExternal", w.BotToken, p, &slot); err != nil {
			return nil, err
		}
		if slot.FileID == "" {
			return nil, fmt.Errorf("slack files.getUploadURLExternal gave no file id")
		}
		if err := w.checkUploadURL(slot.UploadURL); err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, slot.UploadURL, bytes.NewReader(f.Data))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/octet-stream")
		res, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("uploading %s: %w", f.Name, err)
		}
		io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("uploading %s: HTTP %d", f.Name, res.StatusCode)
		}
		shared = append(shared, done{ID: slot.FileID, Title: f.Name})
	}
	list, _ := json.Marshal(shared)
	p := url.Values{"files": {string(list)}, "channel_id": {channel}}
	if thread != "" {
		p.Set("thread_ts", thread)
	}
	if comment != "" {
		p.Set("initial_comment", comment)
	}
	if err := w.call(ctx, "files.completeUploadExternal", w.BotToken, p, nil); err != nil {
		return nil, err
	}
	ids := []string{}
	for _, d := range shared {
		ids = append(ids, d.ID)
	}
	return ids, nil
}
