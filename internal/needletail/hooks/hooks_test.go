package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/core"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engine/needletail"
)

const coreKey = "atlas-core-key-0123456789abcdef"

func logTo(t *testing.T) func(string) { return func(s string) { t.Log(s) } }

// realCore is the core's own ingest (pkg/core Monitor on /ingest/<kind>), keyed for atlas.
func realCore(t *testing.T) (*httptest.Server, *core.Monitor) {
	t.Helper()
	dir := t.TempDir()
	db, err := core.OpenDB(filepath.Join(dir, "core", "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	keys := filepath.Join(dir, "secrets.local.yaml")
	if err := os.WriteFile(keys, []byte("local:\n  agents:\n    atlas:\n      SWARM_CORE_KEY: "+coreKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := core.NewMonitor(db, core.NewAgentKeys(keys), nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/ingest/", m.Handle)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, m
}

func secretRedact(s string) string {
	return strings.ReplaceAll(s, "hunter2-secret-value", "[REDACTED]")
}

// The core takes these hooks as they are: verified with the agent's key, parsed by the adapter,
// the call live under its real id with the file it names, then done and attributed.
func TestTheCoreIngestsTheHooks(t *testing.T) {
	srv, m := realCore(t)
	s := New(Options{URL: srv.URL + "/ingest", Key: coreKey, Redact: secretRedact, Log: logTo(t),
		Session: func(id string) (string, string) { return "root-" + id, "api_server" }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	var files []core.ToolEvent
	var fmu sync.Mutex
	m.OnToolDone(func(e core.ToolEvent) { fmu.Lock(); files = append(files, e); fmu.Unlock() })

	call := wire.ToolCall{ID: "call_Xy12", Name: "write_file", Arguments: `{"path":"/shared/group/notes.md","content":"hi"}`}
	s.TurnStart("s1")
	s.ToolStart("s1", call)
	flush(t, s)
	live := m.Live("atlas")
	if live == nil || live.Tool != "write_file" || live.Running != 1 || live.Sessions != 1 {
		t.Fatalf("live %+v", live)
	}
	running := m.RunningCalls("atlas")
	if len(running) != 1 || running[0].Tool != "write_file" {
		t.Errorf("running %+v", running)
	}
	s.ToolDone("s1", call, `{"ok":true}`, false, 1250*time.Millisecond)
	s.TurnEnd("s1", nil)
	flush(t, s)
	live = m.Live("atlas")
	if live.Running != 0 || live.Sessions != 0 || live.LastTool != "write_file" {
		t.Errorf("after: %+v", live)
	}
	fmu.Lock()
	if len(files) != 1 || files[0].Agent != "atlas" || files[0].Path != "/shared/group/notes.md" || files[0].Tool != "write_file" {
		t.Errorf("attribution %+v", files)
	}
	fmu.Unlock()
	tl, err := m.Timeline("atlas", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	for _, e := range tl {
		events = append(events, e.Event+":"+e.Tool+":"+e.Session+":"+e.Preview)
	}
	want := []string{"session_end::root-s1:api_server", "tool_done:write_file:root-s1:ok 1.25s",
		"tool_start:write_file:root-s1:/shared/group/notes.md", "session_start::root-s1:api_server"}
	if !reflect.DeepEqual(events, want) {
		t.Errorf("timeline %q", events)
	}
}

// One delivery as the adapter reads it: signature header, every field, the hook body v1.
func TestDeliveryContract(t *testing.T) {
	var mu sync.Mutex
	var got []*http.Request
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	at := time.Date(2026, 10, 20, 9, 0, 1, 250e6, time.UTC)
	s := New(Options{URL: srv.URL + "/ingest/", Key: coreKey, Now: func() time.Time { return at },
		Session: func(id string) (string, string) { return id, "slack" }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	s.ToolDone("slack-C1-1.2", wire.ToolCall{ID: "call_abc123", Name: "terminal", Arguments: `{"command":"ls /shared/group"}`}, "", true, 40*time.Millisecond)
	flush(t, s)
	if len(got) != 1 || got[0].URL.Path != "/ingest/needletail" || got[0].Header.Get("Content-Type") != "application/json" {
		t.Fatalf("requests %v", got)
	}
	rt := needletail.NewRuntime()
	sig := rt.HookSignature(got[0].Header)
	if "sha256="+sig != Sign(coreKey, bodies[0]) {
		t.Errorf("signature %q", got[0].Header.Get(wire.HookSignatureHeader))
	}
	e, err := rt.ParseHook(bodies[0])
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Delivery) != 36 || e.Delivery[14] != '7' {
		t.Errorf("delivery %q is not a UUIDv7", e.Delivery)
	}
	want := &engine.HookEvent{Kind: engine.ToolDone, Name: "tool_done", Delivery: e.Delivery, At: at, Session: "slack-C1-1.2",
		Tool: "terminal", ToolInput: map[string]any{"command": "ls /shared/group"}, CallID: "call_abc123", Platform: "slack",
		Status: "error", Duration: 40 * time.Millisecond}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("event %+v", e)
	}
	var raw map[string]any
	_ = json.Unmarshal(bodies[0], &raw)
	keys := []string{}
	for k := range raw {
		keys = append(keys, k)
	}
	if len(keys) != 12 {
		t.Errorf("body keys %v", keys)
	}
}

// Inputs are redacted, cut short and bounded; an input too large keeps only its path.
func TestInputIsRedactedAndBounded(t *testing.T) {
	s := New(Options{Redact: secretRedact})
	in := s.Input(`{"command":"curl -H 'Authorization: Bearer hunter2-secret-value' x","content":"` + strings.Repeat("é", 400) + `",
		"nested":{"a":{"b":{"c":{"d":{"e":"deep"}}}}},"list":[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18]}`)
	if c := in["command"].(string); strings.Contains(c, "hunter2") || !strings.Contains(c, "[REDACTED]") {
		t.Errorf("command %q", c)
	}
	if c := in["content"].(string); len(c) > maxString || !strings.HasSuffix(c, "…") || !utf8Valid(c) {
		t.Errorf("content %d bytes", len(c))
	}
	if l := in["list"].([]any); len(l) != maxItems {
		t.Errorf("list %d", len(l))
	}
	b, _ := json.Marshal(in["nested"])
	if strings.Contains(string(b), "deep") {
		t.Errorf("nesting kept: %s", b)
	}
	big := map[string]any{"path": "/shared/group/big.md"}
	for i := 0; i < maxKeys; i++ {
		big[fmt.Sprintf("k%02d", i)] = []any{strings.Repeat("x", 600), strings.Repeat("y", 600), strings.Repeat("z", 600)}
	}
	raw, _ := json.Marshal(big)
	if got := s.Input(string(raw)); !reflect.DeepEqual(got, map[string]any{"path": "/shared/group/big.md"}) {
		t.Errorf("large input kept %d keys", len(got))
	}
	if got := s.Input("not json"); len(got) != 0 {
		t.Errorf("junk: %v", got)
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "?") == s }

// The core away: deliveries are retried in order with the same body; a 4xx is dropped; an event
// older than MaxAge is dropped; a full queue drops the oldest; emitting never blocks.
func TestRetryOrderAndDrops(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var fails atomic.Int32
	fails.Store(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var h wire.Hook
		_ = json.Unmarshal(b, &h)
		mu.Lock()
		seen = append(seen, h.Tool+"/"+h.Delivery[:8])
		mu.Unlock()
		switch {
		case h.Tool == "bad":
			w.WriteHeader(http.StatusBadRequest)
		case fails.Add(-1) >= 0:
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.WriteHeader(http.StatusAccepted) // the relay holding it counts as delivered
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(Options{URL: srv.URL, Key: coreKey, Wait: time.Millisecond, MaxWait: 2 * time.Millisecond, Log: logTo(t)})
	s.Start(ctx)
	s.ToolStart("s", wire.ToolCall{ID: "1", Name: "first"})
	s.ToolStart("s", wire.ToolCall{ID: "2", Name: "bad"})
	s.ToolStart("s", wire.ToolCall{ID: "3", Name: "third"})
	flush(t, s)
	mu.Lock()
	tools := []string{}
	for _, x := range seen {
		tools = append(tools, strings.Split(x, "/")[0])
	}
	if strings.Join(tools, ",") != "first,first,first,bad,third" || seen[0] != seen[1] || seen[1] != seen[2] {
		t.Errorf("attempts %v", seen)
	}
	mu.Unlock()

	// Nothing answers: the queue keeps the newest, emitting stays instant, old events give up.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	q := New(Options{URL: dead.URL, Key: coreKey, Queue: 3, Wait: time.Millisecond, MaxWait: time.Millisecond, MaxAge: time.Minute, Now: clock})
	started := time.Now()
	for i := 0; i < 10; i++ {
		q.ToolStart("s", wire.ToolCall{ID: fmt.Sprint(i), Name: fmt.Sprintf("t%d", i)})
	}
	if time.Since(started) > time.Second {
		t.Error("emit blocked")
	}
	q.mu.Lock()
	kept := []string{}
	for _, it := range q.queue {
		kept = append(kept, strings.Fields(it.what)[1])
	}
	dropped := q.dropped
	q.mu.Unlock()
	if strings.Join(kept, ",") != "t7,t8,t9" || dropped != 7 {
		t.Errorf("kept %v dropped %d", kept, dropped)
	}
	qctx, qcancel := context.WithCancel(context.Background())
	defer qcancel()
	q.Start(qctx)
	q.signal <- struct{}{}
	mu.Lock()
	now = now.Add(2 * time.Minute) // past MaxAge: each gives up after its next attempt
	mu.Unlock()
	flush(t, q)
	q.mu.Lock()
	if len(q.queue) != 0 {
		t.Errorf("left %d", len(q.queue))
	}
	q.mu.Unlock()
	qcancel()
	select {
	case <-q.Done():
	case <-time.After(2 * time.Second):
		t.Error("worker did not stop")
	}
}

func flush(t *testing.T, s *Sender) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.Flush(ctx)
	if ctx.Err() != nil {
		t.Fatal("hooks not flushed")
	}
}

// A core that refuses every event (a wrong key, say) is logged once and then every 100th time,
// not once per event.
func TestRefusalsAreLoggedSparingly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	var mu sync.Mutex
	lines := []string{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(Options{URL: srv.URL, Key: coreKey, Queue: 300, Log: func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() }})
	for i := range 250 {
		s.ToolStart("s", wire.ToolCall{ID: fmt.Sprint(i), Name: "terminal"})
	}
	s.Start(ctx)
	flush(t, s)
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 3 || !strings.Contains(lines[0], "refused") || !strings.Contains(lines[2], "HTTP 401 (200 refused so far)") {
		t.Errorf("%d lines: %q", len(lines), lines)
	}
}

// A delivery the core takes ends a run of refusals: the next refusal (a key rotated again, say)
// is logged as the first of a new run, not swallowed until the 100th.
func TestRefusalsCountAfreshAfterADelivery(t *testing.T) {
	var refuse atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if refuse.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	var mu sync.Mutex
	lines := []string{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(Options{URL: srv.URL, Key: coreKey, Log: func(l string) { mu.Lock(); lines = append(lines, l); mu.Unlock() }})
	s.Start(ctx)
	refuse.Store(true)
	s.ToolStart("s", wire.ToolCall{ID: "1", Name: "terminal"})
	flush(t, s)
	refuse.Store(false)
	s.ToolStart("s", wire.ToolCall{ID: "2", Name: "terminal"})
	flush(t, s)
	refuse.Store(true)
	s.ToolStart("s", wire.ToolCall{ID: "3", Name: "terminal"})
	flush(t, s)
	mu.Lock()
	defer mu.Unlock()
	n := 0
	for _, l := range lines {
		if strings.Contains(l, "(1 refused so far)") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("lines %q", lines)
	}
}
