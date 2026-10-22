package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/engine"
	adapter "github.com/camfinc/stormo/pkg/engine/needletail"
)

const key = "test-key-0123456789abcdef"

// fakeAgent answers "echo <input>"; "compact" compacts the conversation first; "block" waits for
// release.
type fakeAgent struct {
	store   *sessions.Store
	release chan struct{}
	stuck   chan struct{} // "stuck" waits for it and ignores cancellation
	mu      sync.Mutex
	runs    []string
}

func (f *fakeAgent) Run(ctx context.Context, session, input string, _ loop.Options) (*loop.Result, error) {
	f.mu.Lock()
	f.runs = append(f.runs, session+":"+input)
	f.mu.Unlock()
	tip, ok := f.store.Tip(session)
	if !ok {
		return nil, loop.ErrNoSession
	}
	if input == "block" {
		select {
		case <-f.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if input == "stuck" {
		<-f.stuck
	}
	if input == "fail" {
		return nil, errors.New("model endpoint 0: HTTP 502")
	}
	if input == "compact" {
		rows, _ := f.store.Messages(tip)
		var err error
		if tip, err = f.store.Compact(tip, "summary", rows[len(rows)-2:]); err != nil {
			return nil, err
		}
	}
	_, _ = f.store.Append(tip, wire.Message{Role: "user", Content: input},
		wire.Message{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "call_1", Name: "terminal", Arguments: "{}"}}},
		wire.Message{Role: "tool", ToolCallID: "call_1", Content: "ok"},
		wire.Message{Role: "assistant", Content: "echo " + input})
	return &loop.Result{Text: "echo " + input, Session: tip}, nil
}

func (f *fakeAgent) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runs)
}

func newServer(t *testing.T, state string) (*Server, *fakeAgent, *httptest.Server) {
	t.Helper()
	store, err := sessions.Open(filepath.Join(state, "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAgent{store: store, release: make(chan struct{}), stuck: make(chan struct{})}
	s, err := New(Options{Key: key, Store: store, Agent: fa, StateDir: filepath.Join(state, "state")})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, fa, srv
}

// call is engine.APICall as pkg/local builds it.
func call(srv *httptest.Server, k string) engine.APICall {
	return func(ctx context.Context, method, path string, body any) (int, []byte, error) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, rd)
		req.Header.Set("Authorization", "Bearer "+k)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		return res.StatusCode, raw, err
	}
}

// The adapter's Conversations client is the contract: run it against the real server.
func TestConversationsContract(t *testing.T) {
	_, fa, srv := newServer(t, t.TempDir())
	conv := adapter.NewRuntime().Conversations(call(srv, key))
	ctx := context.Background()

	// Send on a conversation that does not exist: 404, the adapter creates it, then chats
	reply, tip, err := conv.Send(ctx, "c1", "hello")
	if err != nil || reply != "echo hello" || tip != "c1" {
		t.Fatalf("send = %q %q %v", reply, tip, err)
	}
	if id, err := conv.New(ctx, "c1", "again"); err != nil || id != "c1" {
		t.Errorf("new existing = %q %v", id, err)
	}
	if id, err := conv.New(ctx, "", "fresh"); err != nil || !sessions.ValidID(id) {
		t.Errorf("new generated = %q %v", id, err)
	}
	// a compaction moves the tip; chat on the root still reaches it
	reply, tip, err = conv.Send(ctx, "c1", "compact")
	if err != nil || tip == "c1" || reply != "echo compact" {
		t.Fatalf("compact = %q %q %v", reply, tip, err)
	}
	if _, tip2, _ := conv.Send(ctx, "c1", "after"); tip2 != tip {
		t.Errorf("chat on root went to %s, tip is %s", tip2, tip)
	}
	if fa.runs[len(fa.runs)-1] != "c1:after" {
		t.Errorf("runs = %v", fa.runs)
	}

	list, err := conv.List(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var c1 *engine.Conversation
	for i := range list {
		if list[i].ID == "c1" {
			c1 = &list[i]
		}
	}
	if len(list) != 2 || c1 == nil || !c1.Compacted || c1.Messages != 12 || c1.Source != SourceAPI || c1.Preview != "hello" {
		t.Errorf("list = %+v", list)
	}

	tr, err := conv.Transcript(ctx, "c1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Tip != tip || !tr.Compacted || len(tr.Messages) != 12 {
		t.Fatalf("transcript = %+v", tr)
	}
	if m := tr.Messages[1]; m.Kind != engine.MessageTool || m.Tools[0] != "terminal" {
		t.Errorf("tool call row = %+v", m)
	}
	if m := tr.Messages[2]; m.Tool != "terminal" || m.Content != "ok" {
		t.Errorf("tool row = %+v", m)
	}
	if last := tr.Messages[len(tr.Messages)-1]; last.Content != "echo after" {
		t.Errorf("last = %+v", last)
	}
	// limit with order=latest is the newest rows, oldest first
	short, _ := conv.Transcript(ctx, tip, 2)
	if len(short.Messages) != 2 || short.Messages[1].Content != "echo after" {
		t.Errorf("latest 2 = %+v", short.Messages)
	}

	if n, err := conv.Delete(ctx, tip); err != nil || n != 2 {
		t.Errorf("delete = %d %v", n, err)
	}
	if n, err := conv.Delete(ctx, "c1"); err != nil || n != 0 {
		t.Errorf("delete gone = %d %v", n, err)
	}
	if _, err := conv.Transcript(ctx, "c1", 0); !errors.Is(err, engine.ErrNotFound) {
		t.Errorf("transcript gone: %v", err)
	}

	// a failed turn is an error with its message, not a reply
	if _, _, err := conv.Send(ctx, "c2", "fail"); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("fail = %v", err)
	}
	// wrong key
	if _, err := adapter.NewRuntime().Conversations(call(srv, "wrong-key-0123456789")).List(ctx, 1); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong key: %v", err)
	}
}

func TestBusyAndActivity(t *testing.T) {
	s, fa, srv := newServer(t, t.TempDir())
	rt := adapter.NewRuntime()
	conv := rt.Conversations(call(srv, key))
	ctx := context.Background()
	get := func(ctx context.Context, path string) (any, error) {
		_, raw, err := call(srv, key)(ctx, http.MethodGet, path, nil)
		var v any
		_ = json.Unmarshal(raw, &v)
		return v, err
	}
	act, _, err := rt.Activity(ctx, get, nil, time.Now())
	if err != nil || act.ActiveAgents != 0 || act.GatewayBusy || act.RunningJobs == nil || act.LastActive != "" {
		t.Fatalf("idle activity = %+v %v", act, err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _, _, _ = conv.Send(ctx, "a", "block") }()
	waitFor(t, func() bool { return fa.count() == 1 })
	// same conversation: 409; a second one is fine; a third over the cap of two: 429
	if _, _, err := conv.Send(ctx, "a", "x"); !errors.Is(err, engine.ErrBusy) {
		t.Errorf("same conversation: %v", err)
	}
	if n, err := conv.Delete(ctx, "a"); !errors.Is(err, engine.ErrBusy) || n != 0 {
		t.Errorf("delete busy: %d %v", n, err)
	}
	wg.Add(1)
	go func() { defer wg.Done(); _, _, _ = conv.Send(ctx, "b", "block") }()
	waitFor(t, func() bool { return fa.count() == 2 })
	if _, err := conv.New(ctx, "c", ""); err != nil {
		t.Fatal(err)
	}
	status, _, _ := call(srv, key)(ctx, http.MethodPost, "/v1/sessions/c/chat", wire.ChatRequest{Message: "x"})
	if status != http.StatusTooManyRequests {
		t.Errorf("over cap = %d", status)
	}
	act, _, _ = rt.Activity(ctx, get, nil, time.Now())
	if act.ActiveAgents != 2 || !act.GatewayBusy || act.Source != SourceAPI || act.LastActive == "" {
		t.Errorf("busy activity = %+v", act)
	}
	close(fa.release)
	wg.Wait()
	act, _, _ = rt.Activity(ctx, get, nil, time.Now())
	if act.ActiveAgents != 0 || act.GatewayBusy {
		t.Errorf("after = %+v", act)
	}
	s.Shutdown(context.Background())
}

func TestWakeIdempotent(t *testing.T) {
	state := t.TempDir()
	s, fa, srv := newServer(t, state)
	rt := adapter.NewRuntime()
	ctx := context.Background()
	wake := func(s *httptest.Server, idem string) (int, wire.RunAccepted) {
		req, err := rt.WakeRequest(ctx, s.URL, "You have a message.", "swarm-bus-7", idem)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key) // as core.EngineWake does
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var a wire.RunAccepted
		_ = json.NewDecoder(res.Body).Decode(&a)
		return res.StatusCode, a
	}
	code, first := wake(srv, "wake-1")
	if code != http.StatusAccepted || first.RunID == "" || first.SessionID != "swarm-bus-7" {
		t.Fatalf("wake = %d %+v", code, first)
	}
	waitFor(t, func() bool { return fa.count() == 1 })
	s.Shutdown(context.Background()) // the run finished
	code, again := wake(srv, "wake-1")
	if code != http.StatusAccepted || again != first || fa.count() != 1 {
		t.Errorf("retry = %d %+v (runs %d)", code, again, fa.count())
	}
	// remembered across a restart
	_, fa2, srv2 := newServer(t, state)
	if code, again := wake(srv2, "wake-1"); code != http.StatusAccepted || again != first || fa2.count() != 0 {
		t.Errorf("after restart = %d %+v", code, again)
	}
	if code, other := wake(srv2, "wake-2"); code != http.StatusAccepted || other.RunID == first.RunID {
		t.Errorf("new key = %d %+v", code, other)
	}
	waitFor(t, func() bool { return fa2.count() == 1 })
	if raw, err := os.ReadFile(filepath.Join(state, "state", "idem.json")); err != nil || !strings.Contains(string(raw), "wake-2") {
		t.Errorf("idem.json = %s %v", raw, err)
	}
	tr, _ := s.store.Transcript("swarm-bus-7", 0, false)
	if len(tr.Data) == 0 || tr.Data[0].Content != "You have a message." {
		t.Errorf("wake transcript = %+v", tr.Data)
	}
}

func TestHealthAndAuth(t *testing.T) {
	_, _, srv := newServer(t, t.TempDir())
	res, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Errorf("health = %d %s", res.StatusCode, body)
	}
	for _, path := range []string{"/v1/activity", "/v1/sessions"} {
		res, _ := http.Get(srv.URL + path)
		res.Body.Close()
		if res.StatusCode != 401 {
			t.Errorf("%s without key = %d", path, res.StatusCode)
		}
	}
	// a short key serves nothing
	store, _ := sessions.Open(t.TempDir(), nil)
	s, _ := New(Options{Key: "short", Store: store})
	weak := httptest.NewServer(s.Handler())
	defer weak.Close()
	if status, _, _ := call(weak, "short")(context.Background(), http.MethodGet, "/v1/activity", nil); status != 401 {
		t.Errorf("short key = %d", status)
	}
	if status, _, _ := call(srv, key)(context.Background(), http.MethodPost, "/v1/runs", wire.RunRequest{}); status != 400 {
		t.Errorf("empty run = %d", status)
	}
	if status, _, _ := call(srv, key)(context.Background(), http.MethodPost, "/v1/sessions", wire.NewSession{ID: "../x"}); status != 400 {
		t.Errorf("bad id = %d", status)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}

// Shutdown: no turn starts once it has begun, and a turn that ignores its cancellation does not
// hold the process past the grace period.
func TestShutdownBounded(t *testing.T) {
	s, fa, srv := newServer(t, t.TempDir())
	s.grace = 100 * time.Millisecond
	defer close(fa.stuck)
	go func() { _, _ = s.Turn(context.Background(), "a", "stuck", wire.Session{}, loop.Options{}) }()
	waitFor(t, func() bool { return fa.count() == 1 })
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() { s.Shutdown(expired); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Shutdown waits for a stuck turn forever")
	}
	if _, err := s.Turn(context.Background(), "b", "x", wire.Session{}, loop.Options{}); !errors.Is(err, ErrClosing) {
		t.Errorf("turn after shutdown: %v", err)
	}
	ctx := context.Background()
	if _, err := s.store.Create(wire.Session{ID: "c"}); err != nil {
		t.Fatal(err)
	}
	status, _, _ := call(srv, key)(ctx, http.MethodPost, "/v1/sessions/c/chat", wire.ChatRequest{Message: "x"})
	if status != http.StatusServiceUnavailable {
		t.Errorf("chat after shutdown = %d", status)
	}
	status, _, _ = call(srv, key)(ctx, http.MethodPost, "/v1/runs", wire.RunRequest{Input: "x"})
	if status != http.StatusServiceUnavailable {
		t.Errorf("run after shutdown = %d", status)
	}
	if fa.count() != 1 {
		t.Errorf("runs = %v", fa.runs)
	}
}

// Scheduled runs have their own lane (ReserveJob: one turn by default, Options.JobTurns), beside
// the chat's two, so neither starves the other; a reservation makes no session until its turn
// runs, and the job shows in the activity while it runs.
func TestJobLane(t *testing.T) {
	store, err := sessions.Open(filepath.Join(t.TempDir(), "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	fa := &fakeAgent{store: store, release: make(chan struct{}), stuck: make(chan struct{})}
	jobs := []string{"j1"}
	last := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	s, err := New(Options{Key: key, Store: store, Agent: fa, Jobs: func() ([]string, time.Time) { return jobs, last }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	cron := wire.Session{Source: "cron", Title: "Daily digest"}
	r1, err := s.ReserveJob("cron-j1-20261021T090000", cron)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Get("cron-j1-20261021T090000"); ok {
		t.Error("a reservation made the session")
	}
	if _, err := s.ReserveJob("cron-j1-20261021T090000", cron); !errors.Is(err, ErrSessionBusy) {
		t.Errorf("same session: %v", err)
	}
	if _, err := s.ReserveJob("cron-j2-20261021T090000", cron); !errors.Is(err, ErrBusy) {
		t.Errorf("a second job over the lane's one turn: %v", err)
	}
	// the chat's two turns stay free while a job runs, and a job's lane while the chat is full
	c1, err := s.Reserve("chat-1", wire.Session{Source: SourceAPI})
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.Reserve("chat-2", wire.Session{Source: SourceAPI})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reserve("chat-3", wire.Session{Source: SourceAPI}); !errors.Is(err, ErrBusy) {
		t.Errorf("a third chat turn: %v", err)
	}
	c1.Release()
	c2.Release()
	r2, err := s.Reserve("chat-2", wire.Session{Source: SourceAPI})
	if err != nil {
		t.Fatal(err)
	}
	r2.Release()
	r2.Release() // twice is once
	res, err := r1.Turn(context.Background(), "go", loop.Options{})
	if err != nil || res.Text != "echo go" {
		t.Fatalf("turn = %+v %v", res, err)
	}
	if h, ok := store.Get("cron-j1-20261021T090000"); !ok || h.Source != "cron" || h.Title != "Daily digest" {
		t.Errorf("session = %+v %v", h, ok)
	}
	_, raw, _ := call(srv, key)(context.Background(), http.MethodGet, "/v1/activity", nil)
	var act wire.Activity
	_ = json.Unmarshal(raw, &act)
	if act.ActiveTurns != 1 || act.Source != SourceAPI /* the last turn taken */ || len(act.RunningJobs) != 1 || act.RunningJobs[0] != "j1" || act.LastJobAt != "2026-10-21T09:00:00Z" {
		t.Errorf("activity = %+v", act)
	}
	r1.Release()
	s.Shutdown(context.Background())
	if _, err := s.ReserveJob("cron-j4-20261021T090000", cron); !errors.Is(err, ErrClosing) {
		t.Errorf("after shutdown: %v", err)
	}
	// the lane's size is configurable
	s2, _ := New(Options{Key: key, Store: store, Agent: fa, JobTurns: 2})
	for _, id := range []string{"cron-a-1", "cron-b-1"} {
		if _, err := s2.ReserveJob(id, cron); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	if _, err := s2.ReserveJob("cron-c-1", cron); !errors.Is(err, ErrBusy) {
		t.Errorf("a third job: %v", err)
	}
}

// The activity reports the jail the runtime gives the agent's commands, and the adapter passes
// it to the core.
func TestActivityReportsTheJail(t *testing.T) {
	state := t.TempDir()
	store, err := sessions.Open(filepath.Join(state, "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, jail := range []string{"landlock ABI 10", "off: the kernel has no Landlock (before 5.13)", ""} {
		s, err := New(Options{Key: key, Store: store, Agent: &fakeAgent{store: store}, StateDir: filepath.Join(state, "state"), Jail: jail})
		if err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(s.Handler())
		_, raw, err := call(srv, key)(context.Background(), http.MethodGet, "/v1/activity", nil)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		if a := adapter.ActivityFrom(v, time.Now()); a == nil || a.Jail != jail {
			t.Errorf("jail %q: activity %s", jail, raw)
		}
		if jail == "" && strings.Contains(string(raw), `"jail"`) {
			t.Errorf("an empty jail is reported: %s", raw)
		}
	}
}
