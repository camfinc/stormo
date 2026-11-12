// Package stt transcribes voice notes (docs/needletail.md §4.9): ElevenLabs' speech-to-text over
// HTTP, before the turn that carries the note. The request goes to the provider's fixed host
// only (never a redirect, never another host: an override is for tests and must be a loopback
// address), bounded in size and time; the key goes in its header and never into a log or an error.
package stt

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/camfinc/stormo/pkg/elevenlabs"
)

// ElevenLabs is the provider's name in the config (stt.provider), and its API.
const (
	ElevenLabs = "elevenlabs"
	DefaultURL = elevenlabs.DefaultURL
	// KeyEnv is the env var holding the provider's key.
	KeyEnv = elevenlabs.KeyEnv
	// DefaultModel is ElevenLabs' speech-to-text model (model_id).
	DefaultModel = elevenlabs.DefaultSTTModel
	// URLEnv overrides the provider's URL for tests only: it must be a loopback address.
	URLEnv = "NEEDLETAIL_STT_API_URL"
)

// Bounds of a transcription.
const (
	// MaxAudioBytes is the most audio sent (a voice note is far smaller).
	MaxAudioBytes = elevenlabs.MaxAudioBytes
	// MaxTranscript is the most transcript kept (runes).
	MaxTranscript = elevenlabs.MaxTranscript
	// DefaultTimeout bounds one request.
	DefaultTimeout = elevenlabs.DefaultTimeout
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
func CheckURL(raw string) error { return elevenlabs.CheckURL(raw) }

// Transcribe is the transcript of audio (name and mime describe it to the provider).
func (c *Client) Transcribe(ctx context.Context, name, mime string, audio []byte) (string, error) {
	el := &elevenlabs.Client{BaseURL: c.BaseURL, Key: c.Key, HTTP: c.HTTP, Timeout: c.Timeout}
	text, err := el.Transcribe(ctx, elevenlabs.Transcription{Name: name, MIME: mime, Model: c.Model, Language: c.Language, Audio: audio})
	if err != nil {
		return "", fmt.Errorf("speech-to-text: %w", err)
	}
	return text, nil
}
