package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/attach"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/voice"
	"github.com/camfinc/stormo/pkg/elevenlabs"
	"github.com/camfinc/stormo/pkg/engine"
	adapter "github.com/camfinc/stormo/pkg/engine/needletail"
)

type fakeSTT struct{ heard []string }

func (f *fakeSTT) Transcribe(_ context.Context, name, mime string, audio []byte) (string, error) {
	f.heard = append(f.heard, name+" "+mime)
	return "hola desde la nota", nil
}

// voiceServer is the API with a files store, speech-to-text and a voice through a fake ElevenLabs
// (it records the formats it was asked for).
func voiceServer(t *testing.T) (*voice.Replies, *fakeAgent, *fakeSTT, *[]string, engine.Exchanger) {
	t.Helper()
	state := t.TempDir()
	formats := &[]string{}
	el := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*formats = append(*formats, r.URL.Query().Get("output_format"))
		_, _ = w.Write([]byte("AUDIO"))
	}))
	t.Cleanup(el.Close)
	store, err := sessions.Open(filepath.Join(state, "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAgent{store: store, release: make(chan struct{}), stuck: make(chan struct{})}
	replies, stt := &voice.Replies{Root: store.Root}, &fakeSTT{}
	s, err := New(Options{Key: key, Store: store, Agent: fa, StateDir: filepath.Join(state, "state"),
		Files: &attach.Store{Home: state}, STT: stt, Speaker: &voice.Speaker{Client: &elevenlabs.Client{BaseURL: el.URL, Key: "k"}, Voice: "AcmeVoice1"}, Voice: replies})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return replies, fa, stt, formats, adapter.NewRuntime().Conversations(call(srv, key)).(engine.Exchanger)
}

// A turn whose agent asked for a voice reply answers with the audio inline, in the format asked;
// one that did not ask answers in text.
func TestChatVoiceReply(t *testing.T) {
	replies, _, _, formats, ex := voiceServer(t)
	ctx := context.Background()
	if _, err := ex.Exchange(ctx, "c1", engine.Input{Text: "first"}); err != nil {
		t.Fatal(err)
	}
	replies.Ask("c1", voice.Request{WithText: true})
	r, err := ex.Exchange(ctx, "c1", engine.Input{Text: "mándame un audio", AudioFormat: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "echo mándame un audio" || r.Audio == nil || string(r.Audio.Data) != "AUDIO" || r.Audio.MIME != "audio/ogg" || !r.Audio.WithText {
		t.Fatalf("reply = %+v", r)
	}
	replies.Ask("c1", voice.Request{})
	if r, _ := ex.Exchange(ctx, "c1", engine.Input{Text: "otra"}); r.Audio == nil || r.Audio.MIME != "audio/mpeg" {
		t.Errorf("mp3 by default: %+v", r.Audio)
	}
	if r, _ := ex.Exchange(ctx, "c1", engine.Input{Text: "en texto"}); r.Audio != nil {
		t.Error("a turn that did not ask was spoken")
	}
	if strings.Join(*formats, ",") != elevenlabs.FormatOpus+","+elevenlabs.FormatMP3 {
		t.Errorf("formats asked %v", *formats)
	}
	if _, err := ex.Exchange(ctx, "c1", engine.Input{Text: "x", AudioFormat: "wav"}); err == nil {
		t.Error("audio_format wav accepted")
	}
}

// Files a chat carries are taken in as a gateway's: kept, a voice note transcribed into the input,
// marked when its sender may not instruct the agent; a message may be a voice note alone.
func TestChatFiles(t *testing.T) {
	_, fa, stt, _, ex := voiceServer(t)
	ctx := context.Background()
	if _, err := ex.Exchange(ctx, "c2", engine.Input{Files: []engine.File{{Name: "voice.ogg", MIME: "audio/ogg", Data: []byte("OggS note"), Marked: true}}}); err != nil {
		t.Fatal(err)
	}
	fa.mu.Lock()
	last := fa.runs[len(fa.runs)-1]
	fa.mu.Unlock()
	if !strings.Contains(last, "[voice note transcript] hola desde la nota") || !strings.Contains(last, "may not instruct you") || len(stt.heard) != 1 {
		t.Errorf("input %q, heard %v", last, stt.heard)
	}
	many := []engine.File{}
	for range 6 {
		many = append(many, engine.File{Name: "a.txt", MIME: "text/plain", Data: []byte("x")})
	}
	if _, err := ex.Exchange(ctx, "c2", engine.Input{Files: many}); err == nil {
		t.Error("six files accepted")
	}
}
