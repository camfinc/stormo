package slack

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
	"github.com/camfinc/stormo/internal/needletail/voice"
	"github.com/camfinc/stormo/pkg/elevenlabs"
)

// fakeSpeech is ElevenLabs speaking MP3 (or failing when broken).
func fakeSpeech(t *testing.T, broken bool) *voice.Speaker {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken || r.URL.Query().Get("output_format") != elevenlabs.FormatMP3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("ID3 fake mp3"))
	}))
	t.Cleanup(srv.Close)
	return &voice.Speaker{Client: &elevenlabs.Client{BaseURL: srv.URL, Key: "k"}, Voice: "AcmeVoice1"}
}

// A turn whose agent asked for a voice reply posts an MP3 in its conversation, the text as its
// comment when asked; a voice that cannot be made posts the text instead.
func TestSlackVoiceReply(t *testing.T) {
	replies := &voice.Replies{}
	r := newRig(t, func(o *Options) { o.Speaker, o.Voice = fakeSpeech(t, false), replies })
	replies.Ask(DMSessionID(dm), voice.Request{WithText: true})
	r.fake.Send("e1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "dímelo en audio", TS: "1700000900.000100"})
	deadline := time.Now().Add(5 * time.Second)
	for len(r.fake.Shares()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	sh := r.fake.Shares()
	if len(sh) != 1 || sh[0].Channel != dm || sh[0].Comment != "re: dímelo en audio" || string(sh[0].Files["voice-note.mp3"]) != "ID3 fake mp3" {
		t.Fatalf("shares %+v", sh)
	}
	if p := r.fake.Posts(); len(p) != 0 {
		t.Errorf("the text went twice: %+v", p)
	}
}

func TestSlackVoiceReplyFallsBack(t *testing.T) {
	replies := &voice.Replies{}
	r := newRig(t, func(o *Options) { o.Speaker, o.Voice = fakeSpeech(t, true), replies })
	replies.Ask(DMSessionID(dm), voice.Request{})
	r.fake.Send("e1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "hola", TS: "1700000901.000100"})
	p := r.fake.WaitPosts(1, 5*time.Second)
	if p[0].Text != "re: hola" || len(r.fake.Shares()) != 0 {
		t.Errorf("fallback: %+v, shares %d", p, len(r.fake.Shares()))
	}
	r.mu.Lock()
	logs := strings.Join(r.logs, "\n")
	r.mu.Unlock()
	if !strings.Contains(logs, "the voice note was not sent") {
		t.Errorf("not logged: %s", logs)
	}
}
