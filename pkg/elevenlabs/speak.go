package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DefaultSpeechModel is the text-to-speech model when none is named.
const DefaultSpeechModel = "eleven_v4"

// Output formats Speak gives: Ogg Opus (a voice note: Telegram's sendVoice, WhatsApp's PTT) and
// MP3 (an audio file with a player: Slack, the app).
const (
	FormatOpus = "opus_48000_64"
	FormatMP3  = "mp3_44100_128"
)

// MaxSpeechText is the most text one Speak sends (runes; every current model takes it).
const MaxSpeechText = 5000

// Speech is what to say, in which voice and how.
type Speech struct {
	Text, Voice, Model string
	// Stability and Similarity tune the voice (0 to 1; nil: the voice's own).
	Stability, Similarity *float64
	// Format is FormatOpus or FormatMP3 ("": MP3).
	Format string
}

// Audio is spoken text: its bytes, their MIME type, a file name for them, and what the request
// cost in characters (ElevenLabs' character-cost; 0 when it does not say).
type Audio struct {
	Data       []byte
	MIME, Name string
	Chars      int
}

// Speak is s.Text spoken in s.Voice (POST /v1/text-to-speech/{voice}).
func (c *Client) Speak(ctx context.Context, s Speech) (*Audio, error) {
	text := strings.TrimSpace(s.Text)
	switch {
	case text == "":
		return nil, errors.New("speech: nothing to say")
	case utf8.RuneCountInString(text) > MaxSpeechText:
		return nil, fmt.Errorf("speech: over %d characters", MaxSpeechText)
	case !voiceIDRe.MatchString(s.Voice):
		return nil, errors.New("speech: no voice (or not a voice id)")
	}
	format, mime, name := FormatMP3, "audio/mpeg", "voice-note.mp3"
	if s.Format == FormatOpus {
		format, mime, name = FormatOpus, "audio/ogg", "voice-note.ogg"
	}
	model := s.Model
	if model == "" {
		model = DefaultSpeechModel
	}
	body := map[string]any{"text": text, "model_id": model}
	settings := map[string]float64{}
	if s.Stability != nil {
		settings["stability"] = *s.Stability
	}
	if s.Similarity != nil {
		settings["similarity_boost"] = *s.Similarity
	}
	if len(settings) > 0 {
		body["voice_settings"] = settings
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	raw, h, err := c.doHeader(ctx, http.MethodPost, "/v1/text-to-speech/"+s.Voice, url.Values{"output_format": {format}}, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("speech: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("speech: the answer has no audio")
	}
	chars, _ := strconv.Atoi(h.Get("character-cost"))
	return &Audio{Data: raw, MIME: mime, Name: name, Chars: chars}, nil
}
