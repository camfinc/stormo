package telegram

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/telegram/telegramtest"
	"github.com/camfinc/stormo/internal/needletail/voice"
	"github.com/camfinc/stormo/pkg/elevenlabs"
)

// speaker is a voice through a fake ElevenLabs that speaks Ogg Opus, or fails when broken.
func speaker(t *testing.T, broken bool) (*voice.Speaker, *[]string) {
	t.Helper()
	asked := &[]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*asked = append(*asked, r.URL.Path+"?"+r.URL.RawQuery)
		if broken {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("character-cost", "12")
		_, _ = w.Write([]byte("OggS fake opus"))
	}))
	t.Cleanup(srv.Close)
	return &voice.Speaker{Client: &elevenlabs.Client{BaseURL: srv.URL, Key: "k"}, Voice: "AcmeVoice1"}, asked
}

// A turn whose agent asked for a voice reply is answered with a voice note (Ogg Opus, quoting
// nothing in a private chat) and then its text when asked; a turn that did not ask gets text; a
// voice that cannot be made sends the text instead.
func TestVoiceReply(t *testing.T) {
	sp, asked := speaker(t, false)
	replies := &voice.Replies{}
	fake, _, _ := rig(t, func(o *Options) { o.Speaker, o.Voice = sp, replies })
	replies.Ask("telegram-1001", voice.Request{WithText: true})
	fake.Push(telegramtest.Message(1, alice, "private", alice, "Alice", "mándame un audio", nil))
	sent := fake.WaitSent(1, 5*time.Second)
	v := fake.Voices()
	if len(v) != 1 || v[0].Chat != alice || string(v[0].Audio) != "OggS fake opus" || v[0].MIME != "audio/ogg" {
		t.Fatalf("voices %+v", v)
	}
	if len(sent) != 1 || sent[0].Text != "re: mándame un audio" {
		t.Errorf("text with the note %+v", sent)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "/v1/text-to-speech/AcmeVoice1") || !strings.Contains((*asked)[0], "output_format=opus_48000_64") {
		t.Errorf("asked %v", *asked)
	}
	fake.Push(telegramtest.Message(2, alice, "private", alice, "Alice", "y ahora en texto", nil))
	fake.WaitSent(2, 5*time.Second)
	if len(fake.Voices()) != 1 {
		t.Error("a turn that did not ask was spoken")
	}
}

func TestVoiceReplyFallsBackToText(t *testing.T) {
	sp, _ := speaker(t, true)
	replies := &voice.Replies{}
	fake, _, _ := rig(t, func(o *Options) { o.Speaker, o.Voice = sp, replies })
	replies.Ask("telegram-1001", voice.Request{})
	fake.Push(telegramtest.Message(1, alice, "private", alice, "Alice", "hola", nil))
	if sent := fake.WaitSent(1, 5*time.Second); sent[0].Text != "re: hola" || len(fake.Voices()) != 0 {
		t.Errorf("fallback: sent %+v, voices %d", sent, len(fake.Voices()))
	}
}
