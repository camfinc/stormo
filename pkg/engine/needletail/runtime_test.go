package needletail

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/engine"
)

func TestParseHook(t *testing.T) {
	body, err := os.ReadFile("testdata/hook_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.TrimSpace(string(body)))
	rt := NewRuntime()
	mac := hmac.New(sha256.New, []byte("core-key"))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	for name, c := range map[string]struct {
		header, want string
	}{
		"signed":     {"sha256=" + sig, sig},
		"no prefix":  {sig, ""},
		"short":      {"sha256=abc", ""},
		"upper hex":  {"sha256=" + strings.ToUpper(sig), ""},
		"absent":     {"", ""},
		"other algo": {"sha1=" + sig[:40], ""},
	} {
		h := http.Header{}
		if c.header != "" {
			h.Set("X-Needletail-Signature", c.header)
		}
		if got := rt.HookSignature(h); got != c.want {
			t.Errorf("%s: signature %q, want %q", name, got, c.want)
		}
	}
	e, err := rt.ParseHook(body)
	if err != nil {
		t.Fatal(err)
	}
	want := &engine.HookEvent{Kind: engine.ToolDone, Name: "tool_done", Delivery: "0192f3a4-5b6c-7d8e-9f00-112233445566",
		At: time.Date(2026, 10, 12, 9, 0, 1, 250e6, time.UTC), Session: "slack-C0000000001-1791337000.000100", Tool: "write_file",
		ToolInput: map[string]any{"path": "/shared/group/notes.md"}, CallID: "call_abc123", Platform: "slack", Status: "ok",
		Duration: 1250 * time.Millisecond}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("event = %+v", e)
	}
	// Only the parse is tested here: a retried body yields the same delivery id. Verifying the
	// signature and dropping the repeat are the core's (pkg/core TestVerifySignature, TestIngestLiveStateAndTimeline).
	if again, _ := rt.ParseHook(body); again.Delivery != e.Delivery {
		t.Error("delivery id not stable")
	}
	var m map[string]any
	_ = json.Unmarshal(body, &m)
	for name, mutate := range map[string]func(map[string]any){
		"version 2":     func(m map[string]any) { m["v"] = 2 },
		"no delivery":   func(m map[string]any) { delete(m, "delivery") },
		"unknown event": func(m map[string]any) { m["event"] = "llm_call" },
		"bad time":      func(m map[string]any) { m["at"] = "yesterday" },
	} {
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		mutate(c)
		b, _ := json.Marshal(c)
		if _, err := rt.ParseHook(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := rt.ParseHook([]byte("not json")); err == nil {
		t.Error("junk accepted")
	}
}

func TestActivity(t *testing.T) {
	now := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	paths := []string{}
	get := func(_ context.Context, path string) (any, error) {
		paths = append(paths, path)
		var v any
		_ = json.Unmarshal([]byte(`{"active_turns":2,"busy":true,"source":"slack","running_jobs":["Stale deals <b>"],
			"last_job_at":"2026-10-12T08:59:00Z","last_active":"2026-10-12T08:58:30.5Z",
			"platforms":{"slack":{"state":"connected","needs_attention":false},"Bad Name":{"state":"x"}}}`), &v)
		return v, nil
	}
	a, state, err := NewRuntime().Activity(context.Background(), get, nil, now)
	if err != nil || state != nil {
		t.Fatal(err, state)
	}
	want := &engine.Activity{ActiveAgents: 2, GatewayBusy: true, Source: "slack", RunningJobs: []string{"Stale deals b"},
		LastJobAt: "2026-10-12T08:59:00.000Z", LastActive: "2026-10-12T08:58:30.500Z",
		Platforms: map[string]engine.Platform{"slack": {State: "connected"}}, PolledAt: "2026-10-12T09:00:00.000Z"}
	if !reflect.DeepEqual(a, want) {
		t.Errorf("activity = %+v", a)
	}
	if len(paths) != 1 || paths[0] != "/v1/activity" {
		t.Errorf("paths = %v", paths)
	}
	if ActivityFrom(nil, now) != nil {
		t.Error("nil body")
	}
	for _, body := range []string{`[1,2]`, `"ok"`, `{"active_turns":"two"}`} {
		bad := func(context.Context, string) (any, error) {
			var v any
			_ = json.Unmarshal([]byte(body), &v)
			return v, nil
		}
		if a, _, err := NewRuntime().Activity(context.Background(), bad, nil, now); err == nil || a != nil {
			t.Errorf("%s: accepted (%v)", body, a)
		}
	}
}

func TestWakeRequest(t *testing.T) {
	req, err := NewRuntime().WakeRequest(context.Background(), "http://agent:8642", "hello", "s1", "wake-1")
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.URL.String() != "http://agent:8642/v1/runs" || req.Header.Get("Idempotency-Key") != "wake-1" {
		t.Errorf("req = %s %s %v", req.Method, req.URL, req.Header)
	}
	body, _ := io.ReadAll(req.Body)
	var r wire.RunRequest
	if json.Unmarshal(body, &r) != nil || r.Input != "hello" || r.SessionID != "s1" {
		t.Errorf("body = %s", body)
	}
}

func TestAPIKey(t *testing.T) {
	rt := NewRuntime()
	if rt.APIKeyName() != "API_SERVER_KEY" {
		t.Error(rt.APIKeyName())
	}
	if rt.ValidAPIKey(strings.Repeat("k", 15)) || !rt.ValidAPIKey(strings.Repeat("k", 16)) {
		t.Error("16-character rule")
	}
}
