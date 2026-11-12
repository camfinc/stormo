// Package elevenlabs is ElevenLabs' API for an instance's voice connections (docs/instances.md):
// speech to text, the voices of the account and of the Voice Library, the speech models, and what
// a key may do. The CLI asks with the instance's key (the app never holds it); needletail
// transcribes with the agent's. A request goes to the provider's own host only (never a redirect;
// an override is for tests and must be a loopback address), bounded in size and time; the key goes
// in its header and never into a log or an error.
package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	// DefaultURL is ElevenLabs' API.
	DefaultURL = "https://api.elevenlabs.io"
	// KeyEnv is the env var a voice connection's key goes in by default.
	KeyEnv = "ELEVENLABS_API_KEY"
	// DefaultSTTModel is the speech-to-text model (model_id).
	DefaultSTTModel = "scribe_v2"
)

// Bounds.
const (
	// MaxAudioBytes is the most audio sent to be transcribed (a voice note is far smaller).
	MaxAudioBytes = 20 << 20
	// MaxTranscript is the most transcript kept (runes).
	MaxTranscript = 20_000
	// DefaultTimeout bounds one request.
	DefaultTimeout = 2 * time.Minute
	// MaxListed is the most voices one listing gives.
	MaxListed   = 100
	maxResponse = 8 << 20
)

// Client is one key's way to ElevenLabs.
type Client struct {
	// BaseURL is DefaultURL ("" too) or a loopback override (CheckURL).
	BaseURL string
	Key     string
	HTTP    *http.Client
	Timeout time.Duration
}

// CheckURL accepts the provider's own API or, for tests, an http(s) URL on the loopback interface:
// an override can never send the key to another host.
func CheckURL(raw string) error {
	if raw == "" || strings.TrimRight(raw, "/") == DefaultURL {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("the ElevenLabs URL is not an http URL")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("an ElevenLabs URL override must be a loopback address (tests only)")
	}
	return nil
}

// noRedirect: the provider answers where it was asked; a redirect is refused, so the audio and the
// key go nowhere else.
func noRedirect(*http.Request, []*http.Request) error {
	return errors.New("ElevenLabs redirected the request: refused")
}

// APIError is an answer other than success. It carries the HTTP status and ElevenLabs' own status
// word (detail.status: missing_permissions, invalid_api_key, invalid_uid…), never the body.
type APIError struct {
	HTTP   int
	Status string
	// Permission is the one a missing_permissions answer names (voices_read…).
	Permission string
}

func (e *APIError) Error() string {
	switch {
	case e.Status == "missing_permissions" && e.Permission != "":
		return fmt.Sprintf("ElevenLabs answered HTTP %d: the key lacks the %s permission (allow it on the key: ElevenLabs → API keys)", e.HTTP, e.Permission)
	case e.Status == "invalid_api_key":
		return fmt.Sprintf("ElevenLabs answered HTTP %d: the key is not valid", e.HTTP)
	}
	return fmt.Sprintf("ElevenLabs answered HTTP %d", e.HTTP)
}

var (
	statusRe     = regexp.MustCompile(`^[a-z_]{1,64}$`)
	voiceIDRe    = regexp.MustCompile(`^[A-Za-z0-9]{1,64}$`)
	permissionRe = regexp.MustCompile(`permission ([a-z_]{1,64})\b`)
)

// apiError reads what an error answer says about itself: its status word and, when a permission is
// missing, which one (both checked to be plain words, so nothing else of the body is repeated).
func apiError(code int, body []byte) *APIError {
	e := &APIError{HTTP: code}
	var d struct {
		Detail struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"detail"`
	}
	if json.Unmarshal(body, &d) == nil && statusRe.MatchString(d.Detail.Status) {
		e.Status = d.Detail.Status
		if m := permissionRe.FindStringSubmatch(d.Detail.Message); m != nil {
			e.Permission = m[1]
		}
	}
	return e
}

// do makes one request and gives the answer's body on success (2xx), else an *APIError.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader) ([]byte, error) {
	raw, _, err := c.doHeader(ctx, method, path, query, contentType, body)
	return raw, err
}

// doHeader is do with the answer's headers.
func (c *Client) doHeader(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader) ([]byte, http.Header, error) {
	if c.Key == "" {
		return nil, nil, errors.New("no ElevenLabs key")
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultURL
	}
	if err := CheckURL(base); err != nil {
		return nil, nil, err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := strings.TrimRight(base, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, nil, errors.New("the ElevenLabs request could not be made")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("xi-api-key", c.Key)
	hc := &http.Client{CheckRedirect: noRedirect}
	if c.HTTP != nil {
		copy := *c.HTTP
		copy.CheckRedirect = noRedirect
		hc = &copy
	}
	res, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, errors.New("ElevenLabs timed out")
		}
		// the transport's error names the URL (never the key): keep only that it failed
		return nil, nil, errors.New("ElevenLabs could not be reached")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return nil, nil, errors.New("the ElevenLabs answer could not be read")
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, nil, apiError(res.StatusCode, raw)
	}
	return raw, res.Header, nil
}

// Transcription is what to transcribe: the audio, how it is named and typed to the provider, and
// optionally the model and the language (an ISO 639 code; "": detected).
type Transcription struct {
	Name, MIME, Model, Language string
	Audio                       []byte
}

// Transcribe is the transcript of t's audio, at most MaxTranscript runes.
func (c *Client) Transcribe(ctx context.Context, t Transcription) (string, error) {
	if len(t.Audio) == 0 || len(t.Audio) > MaxAudioBytes {
		return "", fmt.Errorf("the audio is empty or over %d MB", MaxAudioBytes>>20)
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	model := t.Model
	if model == "" {
		model = DefaultSTTModel
	}
	_ = w.WriteField("model_id", model)
	if t.Language != "" {
		_ = w.WriteField("language_code", t.Language)
	}
	_ = w.WriteField("tag_audio_events", "false")
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, safeName(t.Name)))
	mime := t.MIME
	if mime == "" {
		mime = "application/octet-stream"
	}
	h.Set("Content-Type", mime)
	part, err := w.CreatePart(h)
	if err != nil {
		return "", err
	}
	_, _ = part.Write(t.Audio)
	if err := w.Close(); err != nil {
		return "", err
	}
	raw, err := c.do(ctx, http.MethodPost, "/v1/speech-to-text", nil, w.FormDataContentType(), &body)
	if err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", errors.New("the speech-to-text answer is not JSON")
	}
	text := strings.TrimSpace(out.Text)
	if r := []rune(text); len(r) > MaxTranscript {
		text = string(r[:MaxTranscript]) + "…"
	}
	return text, nil
}

func safeName(name string) string {
	out := strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' || r > 0x7e {
			return '_'
		}
		return r
	}, name)
	if out == "" {
		out = "audio"
	}
	return out
}
