package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

const braveKey, tavilyKey = "brave-key-0123456789", "tvly-key-0123456789"

// stub providers on loopback: they check what the runtime sends as each provider's docs say.
func stubProviders(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/res/v1/web/search":
			if r.Method != http.MethodGet || r.Header.Get("X-Subscription-Token") != braveKey || r.URL.Query().Get("q") != "lisbon fares" || r.URL.Query().Get("count") != "3" {
				http.Error(w, "bad brave request "+r.URL.RawQuery, 400)
				return
			}
			fmt.Fprint(w, `{"type":"search","web":{"type":"search","results":[{"title":"Fares","url":"https://a.example/f","description":"cheap <strong>fares</strong>"},{"title":"Two","url":"https://b.example/","description":"second"}]}}`)
		case r.URL.Path == "/search":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+tavilyKey || body["query"] != "lisbon fares" || body["max_results"] != float64(3) || body["api_key"] != nil {
				http.Error(w, "bad tavily request", 400)
				return
			}
			fmt.Fprint(w, `{"query":"lisbon fares","results":[{"title":"Fares","url":"https://a.example/f","content":"cheap fares","score":0.9}]}`)
		case r.URL.Path == "/redirect/res/v1/web/search":
			http.Redirect(w, r, "https://elsewhere.example/steal", http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/fail"):
			body, _ := io.ReadAll(r.Body)
			http.Error(w, "upstream says: "+string(body)+r.Header.Get("X-Subscription-Token"), 429)
		}
	}))
}

func TestSearchProviders(t *testing.T) {
	srv := stubProviders(t)
	defer srv.Close()
	brave := &Searcher{Provider: Brave, Key: braveKey, BaseURL: srv.URL}
	res, err := brave.Search(context.Background(), "lisbon fares", 3)
	if err != nil || len(res) != 2 || res[0].Title != "Fares" || res[0].URL != "https://a.example/f" || res[0].Snippet != "cheap fares" {
		t.Fatalf("brave: %v %+v", err, res)
	}
	tavily := &Searcher{Provider: Tavily, Key: tavilyKey, BaseURL: srv.URL}
	if res, err = tavily.Search(context.Background(), "lisbon fares", 3); err != nil || len(res) != 1 || res[0].Snippet != "cheap fares" {
		t.Fatalf("tavily: %v %+v", err, res)
	}
	// no redirect is followed (the key goes nowhere else); errors carry the status, never the body or the key
	if _, err = (&Searcher{Provider: Brave, Key: braveKey, BaseURL: srv.URL + "/redirect"}).Search(context.Background(), "x", 3); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("redirect: %v", err)
	}
	_, err = (&Searcher{Provider: Brave, Key: braveKey, BaseURL: srv.URL + "/fail"}).Search(context.Background(), "x", 3)
	if err == nil || !strings.Contains(err.Error(), "429") || strings.Contains(err.Error(), braveKey) || strings.Contains(err.Error(), "upstream") {
		t.Errorf("failure: %v", err)
	}
	if _, err = (&Searcher{Provider: Brave}).Search(context.Background(), "x", 3); err == nil {
		t.Error("searched without a key")
	}
}

// The provider's URL is fixed; an override (tests) must be a loopback address.
func TestSearchURLOverride(t *testing.T) {
	for _, ok := range []string{"", "http://127.0.0.1:9/", "http://[::1]:8/x", "http://localhost:7"} {
		if err := CheckSearchURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"https://evil.example", "http://10.0.0.1/", "ftp://127.0.0.1/", "http://169.254.169.254/"} {
		if err := CheckSearchURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := (&Searcher{Provider: Tavily, Key: tavilyKey, BaseURL: "https://evil.example"}).Search(context.Background(), "x", 1); err == nil {
		t.Error("searched an override that is not loopback")
	}
}

func TestSearchToolMarksTheRun(t *testing.T) {
	srv := stubProviders(t)
	defer srv.Close()
	tool := SearchTool(&Searcher{Provider: Tavily, Key: tavilyKey, BaseURL: srv.URL, MaxResults: 3}, nil)
	var marked []string
	ctx := messaging.WithQuarantine(context.Background(), func(why string) error { marked = append(marked, why); return nil })
	out, failed := tools.NewRegistry(tool).Call(ctx, &tools.Env{}, SearchToolName, `{"query":"lisbon fares"}`)
	if failed || len(marked) != 1 || !strings.Contains(out, "cheap fares") || !strings.Contains(out, `"untrusted":true`) {
		t.Fatalf("out %s marked %v", out, marked)
	}
	if h := tool.HookArgs(`{"query":"lisbon fares","max_results":2}`); strings.Contains(h, "lisbon") || !strings.Contains(h, "has_query") {
		t.Errorf("hook args %s", h)
	}
}
