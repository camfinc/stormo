package core

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	atlasKey = "atlas-core-key-0123456789"
	novaKey  = "nova-core-key-0123456789"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "core", "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testKeys(t *testing.T) *AgentKeys {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.local.yaml")
	body := fmt.Sprintf("local:\n  agents:\n    atlas:\n      SWARM_CORE_KEY: %s\n    nova:\n      SWARM_CORE_KEY: %s\n", atlasKey, novaKey)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewAgentKeys(path)
}

func sign(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

var deliverySeq int

// hook is a delivery as Hermes' outbound webhooks serialise it.
func hook(event, tool string, input map[string]any, extra map[string]any, at time.Time) []byte {
	deliverySeq++
	if extra == nil {
		extra = map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"hook_event_name": event, "profile": "default", "tool_name": tool, "tool_input": input,
		"session_id": "s1", "cwd": "/opt/data", "extra": extra, "delivery_id": fmt.Sprintf("d%d", deliverySeq),
		"timestamp": at.UTC().Format("2006-01-02T15:04:05.000000Z")})
	return b
}

func post(t *testing.T, m *Monitor, body []byte, sig string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/ingest/hermes", bytes.NewReader(body))
	if sig != "" {
		r.Header.Set("X-Hermes-Signature-256", sig)
	}
	w := httptest.NewRecorder()
	m.Handle(w, r)
	return w.Code
}

func TestOpenDBMigratesOnceAndRefusesNewer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "core.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := db.Version(); v != len(migrations) {
		t.Fatalf("version %d, want %d", v, len(migrations))
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("core.db mode %v, want 0600", info.Mode().Perm())
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", len(migrations)+1)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := OpenDB(path); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("a newer schema must be refused, got %v", err)
	}
	if _, err := db.Exec("SELECT 1"); err == nil {
		t.Error("closed")
	}
}

func TestVerifySignature(t *testing.T) {
	k := testKeys(t)
	body := []byte(`{"x":1}`)
	if a, ok := k.VerifySignature(body, sign(novaKey, body)); !ok || a != "nova" {
		t.Fatalf("got %q %v", a, ok)
	}
	for _, h := range []string{"", sign("someone-elses-key-0123", body), "sha256=zz", sign(novaKey, []byte(`{"x":2}`)), strings.ToUpper(sign(novaKey, body))} {
		if _, ok := k.VerifySignature(body, h); ok {
			t.Errorf("%q must not verify", h)
		}
	}
}

func TestIngestLiveStateAndTimeline(t *testing.T) {
	m := NewMonitor(testDB(t), testKeys(t), func(s string) string { return strings.ReplaceAll(s, "ana@example.com", "[email]") })
	now := time.Now()
	var done []ToolEvent
	m.OnToolDone(func(e ToolEvent) { done = append(done, e) })

	start := hook("pre_tool_call", "write_file", map[string]any{"path": "/shared/group/notes/ana@example.com.md", "content": "secret body"}, map[string]any{"tool_call_id": "c1"}, now)
	if code := post(t, m, start, sign(atlasKey, start)); code != 204 {
		t.Fatalf("pre_tool_call: %d", code)
	}
	l := m.Live("atlas")
	if l == nil || l.Tool != "write_file" || l.Running != 1 || l.Sessions != 1 {
		t.Fatalf("live %+v", l)
	}
	if m.Live("nova") != nil {
		t.Error("nova never posted")
	}
	// A retried delivery is accepted and counted once.
	if code := post(t, m, start, sign(atlasKey, start)); code != 204 {
		t.Fatalf("duplicate: %d", code)
	}
	end := hook("post_tool_call", "write_file", map[string]any{"path": "/shared/group/notes/ana@example.com.md"}, map[string]any{"tool_call_id": "c1", "status": "ok", "duration_ms": 1200.0, "result": "big"}, now.Add(time.Second))
	if code := post(t, m, end, sign(atlasKey, end)); code != 204 {
		t.Fatalf("post_tool_call: %d", code)
	}
	if l := m.Live("atlas"); l.Tool != "" || l.Running != 0 || l.LastTool != "write_file" {
		t.Fatalf("after post: %+v", l)
	}
	if len(done) != 1 || done[0].Agent != "atlas" || done[0].Path != "/shared/group/notes/ana@example.com.md" {
		t.Fatalf("tool done %+v", done)
	}

	rows, err := m.Timeline("atlas", 10, true)
	if err != nil || len(rows) != 2 {
		t.Fatalf("timeline %v %v", rows, err)
	}
	if rows[0].Event != "post_tool_call" || rows[0].Preview != "ok 1.2s" {
		t.Errorf("newest %+v", rows[0])
	}
	if rows[1].Preview != "/shared/group/notes/[email].md" || strings.Contains(rows[1].Preview, "secret body") {
		t.Errorf("preview must be the scrubbed path, never content: %q", rows[1].Preview)
	}
	public, _ := m.Timeline("atlas", 10, false)
	if public[1].Preview != "" || public[1].Session != "" || public[1].Tool != "write_file" {
		t.Errorf("public rows carry only event, tool and time: %+v", public[1])
	}

	// Terminal commands are clipped; other tools show argument names only.
	cmd := hook("pre_tool_call", "terminal", map[string]any{"command": strings.Repeat("echo hi; ", 40)}, map[string]any{"tool_call_id": "c2"}, now.Add(3*time.Second))
	post(t, m, cmd, sign(atlasKey, cmd))
	web := hook("pre_tool_call", "web_search", map[string]any{"query": "client name", "limit": 3.0}, map[string]any{"tool_call_id": "c3"}, now.Add(3*time.Second+time.Millisecond))
	post(t, m, web, sign(atlasKey, web))
	rows, _ = m.Timeline("atlas", 2, true)
	if rows[0].Preview != "limit, query" {
		t.Errorf("web_search preview %q", rows[0].Preview)
	}
	if r := []rune(rows[1].Preview); len(r) != maxPreview || !strings.HasSuffix(rows[1].Preview, "…") {
		t.Errorf("terminal preview not clipped: %d", len(r))
	}
	if l := m.Live("atlas"); l.Running != 2 || l.Tool != "web_search" {
		t.Errorf("newest running call wins: %+v", l)
	}
	if calls := m.RunningCalls("atlas"); len(calls) != 2 {
		t.Errorf("running calls %v", calls)
	}

	// The session ends: its calls are done whether or not their post arrived.
	bye := hook("on_session_end", "", nil, map[string]any{"platform": "slack"}, now.Add(5*time.Second))
	post(t, m, bye, sign(atlasKey, bye))
	if l := m.Live("atlas"); l.Running != 0 || l.Sessions != 0 {
		t.Errorf("after session end %+v", l)
	}

	// Approvals the agent waits on.
	ask := hook("pre_approval_request", "", nil, map[string]any{"approval_id": "a1"}, now)
	post(t, m, ask, sign(atlasKey, ask))
	if l := m.Live("atlas"); l.Waiting != 1 {
		t.Errorf("waiting %+v", l)
	}
	ans := hook("post_approval_response", "", nil, map[string]any{"approval_id": "a1", "choice": "approve"}, now)
	post(t, m, ans, sign(atlasKey, ans))
	if l := m.Live("atlas"); l.Waiting != 0 {
		t.Errorf("answered %+v", l)
	}
}

func TestIngestRefusals(t *testing.T) {
	m := NewMonitor(testDB(t), testKeys(t), nil)
	now := time.Now()
	ok := hook("pre_tool_call", "terminal", map[string]any{"command": "ls"}, nil, now)
	cases := []struct {
		name string
		body []byte
		sig  string
		want int
	}{
		{"unsigned", ok, "", 401},
		{"unknown key", ok, sign("not-a-fleet-key-0123456", ok), 401},
		{"signed by another body", ok, sign(atlasKey, []byte("{}")), 401},
		{"stale", hook("pre_tool_call", "terminal", nil, nil, now.Add(-time.Hour)), "", 400},
		{"not a hook", []byte(`{"hello":"world"}`), "", 400},
	}
	for _, c := range cases {
		sig := c.sig
		if sig == "" && c.want == 400 {
			sig = sign(atlasKey, c.body)
		}
		if got := post(t, m, c.body, sig); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
	if m.Live("atlas") != nil {
		t.Error("refused deliveries must not change live state")
	}
	big := bytes.Repeat([]byte("x"), maxIngestBody+1)
	if got := post(t, m, big, sign(atlasKey, big)); got != 413 {
		t.Errorf("too large: %d", got)
	}
}

func TestLiveExpiresLostCalls(t *testing.T) {
	m := NewMonitor(testDB(t), testKeys(t), nil)
	now := time.Now()
	b := hook("pre_tool_call", "terminal", map[string]any{"command": "sleep 1"}, map[string]any{"tool_call_id": "c"}, now)
	post(t, m, b, sign(atlasKey, b))
	m.now = func() time.Time { return now.Add(callTimeout + time.Minute) }
	if l := m.Live("atlas"); l.Running != 0 {
		t.Errorf("a call whose post never came stops counting: %+v", l)
	}
	if err := m.Prune(); err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return now.Add(RetainActivity + time.Hour) }
	if err := m.Prune(); err != nil {
		t.Fatal(err)
	}
	if rows, _ := m.Timeline("atlas", 10, true); len(rows) != 0 {
		t.Errorf("pruned rows remain: %v", rows)
	}
}

func TestActivityRoutesNeedOwnerOrSelf(t *testing.T) {
	_, inst := fixture(t)
	keys := testKeys(t)
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: keys, OwnerToken: "owner-token-0123456789"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	b := hook("pre_tool_call", "read_file", map[string]any{"path": "/shared/sales/plan.md"}, nil, time.Now())
	req, _ := http.NewRequest(http.MethodPost, base+"/ingest/hermes", bytes.NewReader(b))
	req.Header.Set("X-Hermes-Signature-256", sign(atlasKey, b))
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 204 {
		t.Fatalf("ingest over HTTP: %v %v", r, err)
	}
	get := func(path, auth string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, base+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(body)
	}
	if code, body := get("/api/agents/atlas/timeline", ""); code != 200 || !strings.Contains(body, `"read_file"`) || strings.Contains(body, "plan.md") {
		t.Errorf("timeline %d %s", code, body)
	}
	for _, who := range []string{"", novaKey, "wrong"} {
		if code, _ := get("/api/agents/atlas/activity", who); code != 401 {
			t.Errorf("activity as %q: %d", who, code)
		}
	}
	for _, who := range []string{"owner-token-0123456789", atlasKey} {
		if code, body := get("/api/agents/atlas/activity", who); code != 200 || !strings.Contains(body, "/shared/sales/plan.md") {
			t.Errorf("activity as %q: %d %s", who, code, body)
		}
	}
	if code, body := get("/api/fleet", ""); code != 200 || !strings.Contains(body, `"agents"`) {
		t.Errorf("fleet %d %s", code, body)
	}
}
