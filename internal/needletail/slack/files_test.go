package slack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/attach"
)

// The bot token goes only to Slack's own https hosts (or, under a test's loopback override, to a
// loopback address): never to a lookalike, another port, plain http or a redirect elsewhere.
func TestFileHostAllowed(t *testing.T) {
	real := &Web{}
	for raw, want := range map[string]bool{
		"https://files.slack.com/files-pri/T1-F1/download/a.png": true,
		"https://slack.com/x":                    true,
		"http://files.slack.com/x":               false,
		"https://files.slack.com:8443/x":         false,
		"https://files.slack.com.evil.example/x": false,
		"https://evilslack.com/x":                false,
		"https://127.0.0.1/x":                    false,
	} {
		u, _ := url.Parse(raw)
		if got := real.fileHostAllowed(u); got != want {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
	test := &Web{BaseURL: "http://127.0.0.1:9/api"}
	for raw, want := range map[string]bool{"http://127.0.0.1:5000/files/F1": true, "http://localhost:1/x": true, "https://files.slack.com/x": false, "http://10.0.0.1/x": false} {
		u, _ := url.Parse(raw)
		if got := test.fileHostAllowed(u); got != want {
			t.Errorf("override %s: %v, want %v", raw, got, want)
		}
	}
}

func TestDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			if r.Header.Get("Authorization") != "Bearer tok-0123456789" {
				w.WriteHeader(403)
				return
			}
			_, _ = w.Write([]byte("content"))
		case "/away":
			http.Redirect(w, r, "https://elsewhere.example/steal", http.StatusFound)
		case "/login":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html>"))
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 100)))
		}
	}))
	defer srv.Close()
	w := &Web{BaseURL: srv.URL + "/api", BotToken: "tok-0123456789"}
	if b, err := w.Download(context.Background(), srv.URL+"/ok", 50); err != nil || string(b) != "content" {
		t.Errorf("ok: %q %v", b, err)
	}
	if _, err := w.Download(context.Background(), srv.URL+"/away", 50); err == nil || strings.Contains(err.Error(), "elsewhere") {
		t.Errorf("redirect: %v", err)
	}
	if _, err := w.Download(context.Background(), srv.URL+"/login", 50); err != errLoginPage {
		t.Errorf("login page: %v", err)
	}
	if _, err := w.Download(context.Background(), srv.URL+"/big", 50); err == nil {
		t.Error("oversized download kept")
	}
	if _, err := w.Download(context.Background(), "https://files.slack.com/x", 50); err == nil {
		t.Error("a real host under a test override")
	}
}

func TestFileKind(t *testing.T) {
	for _, c := range []struct {
		f    File
		want string
	}{
		{File{Mimetype: "image/png", URLPrivateDownload: "u"}, attach.KindImage},
		{File{Mimetype: "image/heic", URLPrivateDownload: "u"}, ""},
		{File{Mimetype: "application/pdf", URLPrivateDownload: "u"}, attach.KindPDF},
		{File{Mimetype: "text/csv", URLPrivateDownload: "u"}, attach.KindText},
		{File{Mimetype: "text/plain", Filetype: "text", URLPrivateDownload: "u"}, attach.KindText},
		{File{Mimetype: "application/json", URLPrivateDownload: "u"}, attach.KindText},
		{File{Mimetype: "text/html", Name: "x.html", URLPrivateDownload: "u"}, ""},
		{File{Mimetype: "audio/mp4", Filetype: "m4a", URLPrivateDownload: "u"}, attach.KindAudio},
		{File{Mimetype: "audio/mpeg", Filetype: "mp3", URLPrivateDownload: "u"}, attach.KindAudio},
		{File{Mimetype: "video/mp4", Filetype: "mp4", Subtype: "slack_audio", URLPrivateDownload: "u"}, attach.KindAudio},
		{File{Mimetype: "video/mp4", Filetype: "mp4", URLPrivateDownload: "u"}, ""},
		{File{Mimetype: "image/png", Mode: "tombstone", URLPrivateDownload: "u"}, ""},
		{File{Mimetype: "image/png", Mode: "external", URLPrivateDownload: "u"}, ""},
		{File{Mimetype: "image/png"}, ""},
	} {
		if got, why := fileKind(c.f); got != c.want || got == "" && why == "" {
			t.Errorf("%+v: %q (%s), want %q", c.f, got, why, c.want)
		}
	}
}
