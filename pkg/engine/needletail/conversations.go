package needletail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/attach"
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
	r, err := c.Exchange(ctx, id, engine.Input{Text: message})
	if err != nil {
		return "", "", err
	}
	return r.Text, r.Tip, nil
}

// Exchange is Send with files in and the agent's voice note out (wire.ChatRequest's files and
// audio_format, wire.ChatResponse's message.audio).
func (c conversations) Exchange(ctx context.Context, id string, m engine.Input) (*engine.Reply, error) {
	req := wire.ChatRequest{Message: m.Text, AudioFormat: m.AudioFormat, Files: chatFiles(ctx, m.Files)}
	var out wire.ChatResponse
	_, err := c.do(ctx, http.MethodPost, sessionPath(id)+"/chat", req, &out)
	if err == engine.ErrNotFound {
		if _, err := c.New(ctx, id, ""); err != nil {
			return nil, err
		}
		_, err = c.do(ctx, http.MethodPost, sessionPath(id)+"/chat", req, &out)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	r := &engine.Reply{Text: out.Message.Content, Tip: id}
	if out.SessionID != "" {
		r.Tip = out.SessionID
	}
	if a := out.Message.Audio; a != nil && len(a.Data) > 0 {
		r.Audio = &engine.Audio{MIME: a.MIME, Name: a.Name, Data: a.Data, WithText: a.WithText}
	}
	return r, nil
}

var _ engine.Exchanger = conversations{}

// filesRoom is what a request's files may hold (their bytes: base64 in JSON is 4/3 of it) within
// the runtime's request bound, with room for the message. A var: tests lower it.
var filesRoom = (wire.MaxRequestBytes - 256<<10) * 3 / 4

// chatFiles is files as a request carries them. Files that would not fit filesRoom (a phone's
// full-size photo, two of them) have their largest images sent as the model is shown them
// (attach.PrepareImage: at most attach.MaxSide px, re-encoded) until they do, so the request is not
// refused for them; what still does not fit goes as it is, for the runtime to refuse.
func chatFiles(ctx context.Context, files []engine.File) []wire.ChatFile {
	var out []wire.ChatFile
	total := 0
	for _, f := range files {
		out = append(out, wire.ChatFile{Name: f.Name, MIME: f.MIME, Data: f.Data, Marked: f.Marked})
		total += len(f.Data)
	}
	tried := make([]bool, len(out))
	for total > filesRoom {
		i := -1
		for j, f := range out {
			if !tried[j] && attach.ShowableImage(strings.ToLower(f.MIME)) && (i < 0 || len(f.Data) > len(out[i].Data)) {
				i = j
			}
		}
		if i < 0 {
			break
		}
		tried[i] = true
		data, mime := out[i].Data, out[i].MIME
		if img, err := attach.PrepareImageCtx(ctx, data); err == nil && len(img.Data) < len(data) {
			data, mime = img.Data, img.MIME
		} else if b, m, err := reencoded(data); err == nil && len(b) < len(data) {
			// within the model's bounds already (PrepareImage gives it back as it is), yet too large
			// for the request with the others
			data, mime = b, m
		}
		total -= len(out[i].Data) - len(data)
		out[i].Data, out[i].MIME = data, mime
	}
	return out
}

// reencoded is an image as a smaller file: at most attach.MaxSide on its longest side, a JPEG at
// quality 75 (PNG when it has transparency). Refused past 50 megapixels, before it is decoded.
func reencoded(data []byte) ([]byte, string, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 50_000_000 {
		return nil, "", fmt.Errorf("the image is %dx%d, too large to decode", cfg.Width, cfg.Height)
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", err
	}
	dst := attach.Downscale(src, attach.MaxSide)
	var buf bytes.Buffer
	if o, ok := dst.(interface{ Opaque() bool }); ok && !o.Opaque() {
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, dst)
		return buf.Bytes(), "image/png", err
	}
	err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 75})
	return buf.Bytes(), "image/jpeg", err
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

// Restart is a person's /new on a channel the core runs (POST /v1/sessions/{id}/new). The route
// answers a conversation never started with 200, so a 404 is a runtime from before it.
func (c conversations) Restart(ctx context.Context, id, by string) error {
	_, err := c.do(ctx, http.MethodPost, sessionPath(id)+"/new", map[string]string{"by": by}, nil)
	if errors.Is(err, engine.ErrNotFound) {
		return engine.ErrUnsupported
	}
	return err
}

// Trust is an approver's /trust on a channel the core runs (POST /v1/sessions/{id}/trust). A
// runtime from before the route took who asked does not say whether it was marked: it trusted it
// all the same, so it counts as marked.
func (c conversations) Trust(ctx context.Context, id, by string) (bool, error) {
	var out struct {
		Marked *bool `json:"marked"`
	}
	_, err := c.do(ctx, http.MethodPost, sessionPath(id)+"/trust", map[string]string{"by": by}, &out)
	return out.Marked == nil || *out.Marked, err
}

// EndorsedSince asks whether what conversation id held at at has been endorsed since (GET
// /v1/sessions/{id}/endorsed). A runtime from before the route answers 404: unsupported, so the
// core keeps the mark.
func (c conversations) EndorsedSince(ctx context.Context, id string, at time.Time) (bool, string, error) {
	var out struct {
		Endorsed bool   `json:"endorsed"`
		By       string `json:"by"`
	}
	_, err := c.do(ctx, http.MethodGet, sessionPath(id)+"/endorsed?at="+url.QueryEscape(at.UTC().Format(time.RFC3339Nano)), nil, &out)
	if errors.Is(err, engine.ErrNotFound) {
		return false, "", engine.ErrUnsupported
	}
	if err != nil {
		return false, "", err
	}
	return out.Endorsed, out.By, nil
}

var _ engine.ConversationCommands = conversations{}
var _ engine.Endorsements = conversations{}
