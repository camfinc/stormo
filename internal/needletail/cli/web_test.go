package cli

import (
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/secrets"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// web_search is offered only with a provider and its key; a test URL override must be loopback.
func TestStartSearch(t *testing.T) {
	cfg := &config.Config{}
	cfg.Web = &wire.WebConfig{Search: &wire.WebSearchConfig{Provider: "brave", MaxResults: 7}}
	environ := []string{"BRAVE_SEARCH_API_KEY=brave-key-0123456789"}
	sec := secrets.Load(&cfg.Config, environ)
	var logs []string
	logf := func(s string) { logs = append(logs, s) }
	env := func(v string) func(string) string { return func(string) string { return v } }
	if s := startSearch(cfg, sec, env(""), logf); s == nil || s.Key != "brave-key-0123456789" || s.MaxResults != 7 || s.BaseURL != "" {
		t.Fatalf("searcher %+v", s)
	}
	if s := startSearch(cfg, sec, env("http://127.0.0.1:9"), logf); s == nil || s.BaseURL != "http://127.0.0.1:9" {
		t.Errorf("loopback override: %+v", s)
	}
	if s := startSearch(cfg, sec, env("https://evil.example"), logf); s != nil {
		t.Error("a non-loopback override was taken")
	}
	if s := startSearch(cfg, secrets.Load(&cfg.Config, nil), env(""), logf); s != nil {
		t.Error("no key, still a searcher")
	}
	if s := startSearch(&config.Config{}, sec, env(""), logf); s != nil {
		t.Error("no provider, still a searcher")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "BRAVE_SEARCH_API_KEY is missing") || strings.Contains(strings.Join(logs, "\n"), "brave-key") {
		t.Errorf("logs %v", logs)
	}
}
