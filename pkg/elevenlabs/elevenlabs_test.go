package elevenlabs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fakeKey = "sk_test_never_in_an_error"

// fake is ElevenLabs as this package uses it: denied lists the permissions the key lacks.
func fake(t *testing.T, denied ...string) *httptest.Server {
	t.Helper()
	deny := func(w http.ResponseWriter, perm string) bool {
		for _, d := range denied {
			if d == perm {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"detail":{"type":"authentication_error","code":"unauthorized","message":"The API key you used is missing the permission ` + perm + ` to execute this operation.","status":"missing_permissions"}}`))
				return true
			}
		}
		return false
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("xi-api-key") != fakeKey {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":{"message":"Invalid API key","status":"invalid_api_key"}}`))
			return
		}
		switch {
		case r.URL.Path == "/v1/speech-to-text":
			if deny(w, "speech_to_text") {
				return
			}
			if _, _, err := r.FormFile("file"); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"detail":{"status":"invalid_parameters","message":"Must provide either file or a URL parameter."}}`))
				return
			}
			_, _ = w.Write([]byte(`{"text":"  hola  "}`))
		case strings.HasPrefix(r.URL.Path, "/v1/text-to-speech/"):
			if deny(w, "text_to_speech") {
				return
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":{"status":"invalid_uid","message":"An invalid ID has been received"}}`))
		case r.URL.Path == "/v2/voices":
			if deny(w, "voices_read") {
				return
			}
			if r.URL.Query().Get("search") == "none" {
				_, _ = w.Write([]byte(`{"voices":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"voices":[
				{"voice_id":"Val1","name":"Ada","category":"professional","labels":{"gender":"female","age":"young","accent":"british","descriptive":"warm","use_case":"conversational","language":"es"},"preview_url":"https://storage.example/val.mp3"},
				{"voice_id":"Ale2","name":"Ben","category":"professional","labels":{"gender":"male","age":"middle_aged","accent":"british","language":"es"}},
				{"voice_id":"","name":"broken"}]}`))
		case r.URL.Path == "/v1/shared-voices":
			if deny(w, "voices_read") {
				return
			}
			q := r.URL.Query()
			if q.Get("gender") != "female" || q.Get("language") != "es" || q.Get("page_size") != "5" {
				t.Errorf("library query = %v", q)
			}
			_, _ = w.Write([]byte(`{"voices":[{"public_owner_id":"own1","voice_id":"Lib1","name":"Cleo","gender":"female","age":"young","accent":"american","language":"es","use_case":"conversational","preview_url":"https://storage.example/lucia.mp3"}],"has_more":false}`))
		case strings.HasPrefix(r.URL.Path, "/v1/voices/add/"):
			if deny(w, "voices_write") {
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":{"status":"voice_not_found","message":"no such voice"}}`))
		case r.URL.Path == "/v1/models":
			if deny(w, "models_read") {
				return
			}
			_, _ = w.Write([]byte(`[{"model_id":"eleven_v4","name":"Eleven v4","can_do_text_to_speech":true,"maximum_text_length_per_request":10000,"languages":[{"language_id":"es"},{"language_id":"en"}]},
				{"model_id":"scribe_v2","name":"Scribe","can_do_text_to_speech":false}]`))
		case r.URL.Path == "/v1/user/subscription":
			if deny(w, "user_read") {
				return
			}
			_, _ = w.Write([]byte(`{"character_count":1200,"character_limit":100000}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestTranscribe(t *testing.T) {
	c := &Client{BaseURL: fake(t).URL, Key: fakeKey}
	text, err := c.Transcribe(context.Background(), Transcription{Name: "voice.ogg", MIME: "audio/ogg", Audio: []byte("OggS"), Language: "es"})
	if err != nil || text != "hola" {
		t.Errorf("transcript %q %v", text, err)
	}
	if _, err := c.Transcribe(context.Background(), Transcription{}); err == nil {
		t.Error("empty audio sent")
	}
	if _, err := (&Client{BaseURL: c.BaseURL}).Transcribe(context.Background(), Transcription{Audio: []byte("x")}); err == nil {
		t.Error("no key: transcribed")
	}
}

// The account's voices are filtered by their labels here; the library's by ElevenLabs.
func TestVoices(t *testing.T) {
	c := &Client{BaseURL: fake(t).URL, Key: fakeKey}
	all, err := c.Voices(context.Background(), Query{})
	if err != nil || len(all) != 2 || !all[0].InAccount || all[0].UseCase != "conversational" || all[0].PreviewURL == "" {
		t.Fatalf("voices = %+v %v", all, err)
	}
	f, err := c.Voices(context.Background(), Query{Gender: "Female", Accent: "British"})
	if err != nil || len(f) != 1 || f[0].ID != "Val1" {
		t.Errorf("filtered = %+v %v", f, err)
	}
	if one, _ := c.Voices(context.Background(), Query{Limit: 1}); len(one) != 1 {
		t.Errorf("limit 1 = %+v", one)
	}
	if none, err := c.Voices(context.Background(), Query{Search: "none"}); err != nil || none == nil || len(none) != 0 {
		t.Errorf("none = %#v %v", none, err)
	}
	lib, err := c.Library(context.Background(), Query{Gender: "female", Language: "es", Limit: 5})
	if err != nil || len(lib) != 1 || lib[0].InAccount || lib[0].Owner != "own1" || lib[0].Accent != "american" {
		t.Errorf("library = %+v %v", lib, err)
	}
	models, err := c.Models(context.Background())
	if err != nil || len(models) != 1 || models[0].ID != "eleven_v4" || models[0].MaxChars != 10000 || len(models[0].Languages) != 2 {
		t.Errorf("models = %+v %v", models, err)
	}
}

// A missing permission is named, with the HTTP status; the key and the rest of the body never are.
func TestErrors(t *testing.T) {
	srv := fake(t, "voices_read")
	_, err := (&Client{BaseURL: srv.URL, Key: fakeKey}).Voices(context.Background(), Query{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Permission != "voices_read" || !strings.Contains(err.Error(), "401") ||
		!strings.Contains(err.Error(), "lacks the voices_read permission") || strings.Contains(err.Error(), "execute this operation") {
		t.Errorf("missing permission: %v", err)
	}
	_, err = (&Client{BaseURL: srv.URL, Key: "sk_wrong"}).Models(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not valid") || strings.Contains(err.Error(), "sk_wrong") {
		t.Errorf("bad key: %v", err)
	}
	hit := false
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer elsewhere.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := (&Client{BaseURL: redirect.URL, Key: fakeKey}).Models(context.Background()); err == nil || hit || strings.Contains(err.Error(), fakeKey) {
		t.Errorf("redirect: %v (followed %v)", err, hit)
	}
	for _, u := range []string{"https://api.elevenlabs.io.evil.example", "http://10.0.0.1", "ftp://127.0.0.1"} {
		if CheckURL(u) == nil {
			t.Errorf("override %s accepted", u)
		}
		if _, err := (&Client{BaseURL: u, Key: fakeKey}).Models(context.Background()); err == nil {
			t.Errorf("override %s used", u)
		}
	}
	for _, u := range []string{"", DefaultURL, DefaultURL + "/", "http://127.0.0.1:9", "http://localhost:9"} {
		if err := CheckURL(u); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { time.Sleep(300 * time.Millisecond) }))
	defer slow.Close()
	if _, err := (&Client{BaseURL: slow.URL, Key: fakeKey, Timeout: 50 * time.Millisecond}).Models(context.Background()); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("slow: %v", err)
	}
}

// Check finds what the key may do without spending: every refusal but missing_permissions is a yes.
func TestCheck(t *testing.T) {
	srv := fake(t, "voices_read", "voices_write", "models_read", "user_read")
	r, err := (&Client{BaseURL: srv.URL, Key: fakeKey}).Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range r.Permissions {
		got[p.Name] = p.Allowed
		if p.For == "" {
			t.Errorf("%s says nothing of what it is for", p.Name)
		}
	}
	want := map[string]bool{"speech_to_text": true, "text_to_speech": true, "voices_read": false, "voices_write": false, "models_read": false, "user_read": false}
	for k, v := range want {
		if a, ok := got[k]; !ok || a != v {
			t.Errorf("%s = %v (present %v), want %v", k, a, ok, v)
		}
	}
	if r.Characters != nil {
		t.Errorf("characters without user_read: %+v", r.Characters)
	}
	r, err = (&Client{BaseURL: fake(t).URL, Key: fakeKey}).Check(context.Background())
	if err != nil || r.Characters == nil || r.Characters.Used != 1200 || r.Characters.Limit != 100000 {
		t.Errorf("full key: %+v %v", r, err)
	}
	for _, p := range r.Permissions {
		if !p.Allowed {
			t.Errorf("full key lacks %s", p.Name)
		}
	}
	if _, err := (&Client{BaseURL: srv.URL, Key: "sk_wrong"}).Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Errorf("bad key: %v", err)
	}
}
