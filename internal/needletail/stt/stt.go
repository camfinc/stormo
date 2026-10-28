// Package stt transcribes voice notes (docs/needletail.md §4.9): ElevenLabs' speech-to-text over
// HTTP, before the turn that carries the note. The request goes to the provider's fixed host
// only (never a redirect, never another host: an override is for tests and must be a loopback
// address), bounded in size and time; the key goes in its header and never into a log or an error.
package stt

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
	"strings"
	"time"
)

// ElevenLabs is the provider's name in the config (stt.provider), and its API.
const (
	ElevenLabs = "elevenlabs"
	DefaultURL = "https://api.elevenlabs.io"
	// KeyEnv is the env var holding the provider's key.
	KeyEnv = "ELEVENLABS_API_KEY"
	// DefaultModel is ElevenLabs' speech-to-text model (model_id).
	DefaultModel = "scribe_v2"
	// URLEnv overrides the provider's URL for tests only: it must be a loopback address.
	URLEnv = "NEEDLETAIL_STT_API_URL"
)

// Bounds of a transcription.
const (
	// MaxAudioBytes is the most audio sent (a voice note is far smaller).
	MaxAudioBytes = 20 << 20
	// MaxTranscript is the most transcript kept (runes).
	MaxTranscript = 20_000
	// DefaultTimeout bounds one request.
	DefaultTimeout = 2 * time.Minute
	maxResponse    = 8 << 20
)

// Client is the provider's speech-to-text.
type Client struct {
	// BaseURL is DefaultURL ("" too) or a loopback override (CheckURL).
	BaseURL  string
	Key      string
	Model    string
	Language string
	HTTP     *http.Client
	Timeout  time.Duration
}

// CheckURL accepts the provider's own API or, for tests, an http(s) URL on the loopback interface:
// an override can never send the key to another host.
func CheckURL(raw string) error {
	if raw == "" || raw == DefaultURL {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("speech-to-text URL is not an http URL")
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("speech-to-text URL override must be a loopback address (tests only)")
	}
	return nil
}

// noRedirect: the provider answers where it was asked; a redirect is refused, so the audio and the
// key go nowhere else.
func noRedirect(*http.Request, []*http.Request) error {
	return errors.New("the speech-to-text service redirected the request: refused")
}

// Transcribe is the transcript of audio (name and mime describe it to the provider).
func (c *Client) Transcribe(ctx context.Context, name, mime string, audio []byte) (string, error) {
	if c.Key == "" {
		return "", errors.New("speech-to-text has no key")
	}
	if len(audio) == 0 || len(audio) > MaxAudioBytes {
		return "", fmt.Errorf("the audio is empty or over %d MB", MaxAudioBytes>>20)
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultURL
	}
	if err := CheckURL(base); err != nil {
		return "", err
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	model := c.Model
	if model == "" {
		model = DefaultModel
	}
	_ = w.WriteField("model_id", model)
	if c.Language != "" {
		_ = w.WriteField("language_code", c.Language)
	}
	_ = w.WriteField("tag_audio_events", "false")
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, safeName(name)))
	if mime == "" {
		mime = "application/octet-stream"
	}
	h.Set("Content-Type", mime)
	part, err := w.CreatePart(h)
	if err != nil {
		return "", err
	}
	_, _ = part.Write(audio)
	if err := w.Close(); err != nil {
		return "", err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/speech-to-text", &body)
	if err != nil {
		return "", errors.New("speech-to-text request could not be made")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
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
			return "", errors.New("speech-to-text timed out")
		}
		// the transport's error names the URL (never the key): keep only that it failed
		return "", errors.New("speech-to-text could not be reached")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return "", errors.New("speech-to-text answer could not be read")
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("speech-to-text answered HTTP %d", res.StatusCode)
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", errors.New("speech-to-text answer is not JSON")
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
