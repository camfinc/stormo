package models

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
)

func TestList(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"data":[{"id":"z-model","context_length":8000},{"id":"a-model","display_name":"A"}]}`))
	}))
	defer srv.Close()
	ctx := context.Background()
	openai := instance.Connection{Name: "openai", Kind: instance.KindOpenAI, BaseURL: srv.URL + "/v1/", Key: "OPENAI_API_KEY"}
	got, err := List(ctx, openai, "sk-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a-model" || got[0].Name != "A" || got[1].ContextLength != 8000 || seen.Get("Authorization") != "Bearer sk-1" {
		t.Errorf("%+v %v", got, seen)
	}
	if _, err := List(ctx, openai, ""); err == nil {
		t.Error("an OpenAI connection without a key must say so")
	}
	anthropic := instance.Connection{Name: "anthropic", Kind: instance.KindAnthropic, BaseURL: srv.URL + "/v1/", Key: "ANTHROPIC_API_KEY"}
	if _, err := List(ctx, anthropic, "ak"); err != nil || seen.Get("x-api-key") != "ak" || seen.Get("Authorization") != "" {
		t.Errorf("anthropic: %v %v", err, seen)
	}
	or := instance.Connection{Name: "openrouter", Kind: instance.KindOpenRouter, BaseURL: srv.URL + "/v1", Key: "OPENROUTER_API_KEY"}
	if _, err := List(ctx, or, ""); err != nil || seen.Get("Authorization") != "" {
		t.Errorf("openrouter's catalog is public: %v", err)
	}
	if _, err := List(ctx, instance.Connection{Name: "chatgpt", Kind: instance.KindChatGPT}, ""); err == nil {
		t.Error("chatgpt models come from the core")
	}
}

func TestComplete(t *testing.T) {
	var auth, path string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		body, _ = io.ReadAll(r.Body)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"verdict\":\"approve\"}"}}]}`))
	}))
	defer srv.Close()
	c := instance.Connection{Name: "openrouter", Kind: instance.KindOpenRouter, BaseURL: srv.URL + "/api/v1/", Key: "OPENROUTER_API_KEY"}
	got, err := Complete(context.Background(), c, "sk-1", "m-1", "rules", "change")
	if err != nil || got != `{"verdict":"approve"}` {
		t.Fatalf("%q %v", got, err)
	}
	if auth != "Bearer sk-1" || path != "/api/v1/chat/completions" || !strings.Contains(string(body), `"stream":false`) || !strings.Contains(string(body), `"model":"m-1"`) {
		t.Errorf("%s %s %s", auth, path, body)
	}
	if _, err := Complete(context.Background(), c, "", "m-1", "s", "u"); err == nil {
		t.Error("no key: refused")
	}
	if got, err := CompletionText([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}]}`)); err != nil || got != "ab" {
		t.Errorf("content parts: %q %v", got, err)
	}
}
