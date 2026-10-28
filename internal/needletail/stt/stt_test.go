package stt

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fakeKey = "fake-stt-key-for-tests-0123456789"

// The request is ElevenLabs' (multipart file, model_id, language_code, the key in xi-api-key) and
// the transcript is its text.
func TestTranscribe(t *testing.T) {
	var got struct{ key, model, lang, file, name, mime string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/speech-to-text" {
			http.NotFound(w, r)
			return
		}
		got.key = r.Header.Get("xi-api-key")
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		got.model, got.lang = r.FormValue("model_id"), r.FormValue("language_code")
		f, h, err := r.FormFile("file")
		if err == nil {
			b, _ := io.ReadAll(f)
			got.file, got.name, got.mime = string(b), h.Filename, h.Header.Get("Content-Type")
		}
		_, _ = w.Write([]byte(`{"language_code":"es","text":"  hola, ¿me confirmas la reserva?  ","words":[]}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Key: fakeKey, Language: "es"}
	text, err := c.Transcribe(context.Background(), "audio_message.m4a", "audio/mp4", []byte("AUDIO"))
	if err != nil {
		t.Fatal(err)
	}
	if text != "hola, ¿me confirmas la reserva?" {
		t.Errorf("text %q", text)
	}
	if got.key != fakeKey || got.model != DefaultModel || got.lang != "es" || got.file != "AUDIO" || got.name != "audio_message.m4a" || got.mime != "audio/mp4" {
		t.Errorf("request %+v", got)
	}
}

// Failures are errors without the key or the body; a redirect is refused (the key never follows
// it); an override that is not loopback is refused before anything is sent; a slow provider
// times out.
func TestTranscribeFailures(t *testing.T) {
	hit := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte(`{"text":"stolen"}`))
	}))
	defer elsewhere.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/v1/speech-to-text", http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c := &Client{BaseURL: redirect.URL, Key: fakeKey}
	if _, err := c.Transcribe(context.Background(), "a.m4a", "audio/mp4", []byte("x")); err == nil || hit || strings.Contains(err.Error(), fakeKey) {
		t.Errorf("redirect: %v (followed %v)", err, hit)
	}
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"detail":{"status":"invalid_api_key","message":"key ` + fakeKey + ` is invalid"}}`))
	}))
	defer denied.Close()
	c.BaseURL = denied.URL
	if _, err := c.Transcribe(context.Background(), "a.m4a", "audio/mp4", []byte("x")); err == nil || strings.Contains(err.Error(), fakeKey) || !strings.Contains(err.Error(), "401") {
		t.Errorf("401: %v", err)
	}
	for _, u := range []string{"https://api.example.com", "http://10.0.0.1:8080", "ftp://127.0.0.1"} {
		if CheckURL(u) == nil {
			t.Errorf("override %s accepted", u)
		}
		c.BaseURL = u
		if _, err := c.Transcribe(context.Background(), "a.m4a", "audio/mp4", []byte("x")); err == nil {
			t.Errorf("override %s used", u)
		}
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	c = &Client{BaseURL: slow.URL, Key: fakeKey, Timeout: 200 * time.Millisecond}
	if _, err := c.Transcribe(context.Background(), "a.m4a", "audio/mp4", []byte("x")); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("slow: %v", err)
	}
	if _, err := (&Client{BaseURL: slow.URL}).Transcribe(context.Background(), "a", "audio/mp4", []byte("x")); err == nil {
		t.Error("no key: transcribed")
	}
	if _, err := c.Transcribe(context.Background(), "a", "audio/mp4", make([]byte, MaxAudioBytes+1)); err == nil {
		t.Error("oversized audio sent")
	}
}
