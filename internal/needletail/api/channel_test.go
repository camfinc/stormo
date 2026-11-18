package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

const coreKey = "core-key-0123456789abcdef"

// channelAgent records each turn's execution and options; "act" makes a tool call, "block" waits.
type channelAgent struct {
	store   *sessions.Store
	release chan struct{}
	mu      sync.Mutex
	seen    []string
}

func (a *channelAgent) Run(ctx context.Context, session, input string, opts loop.Options) (*loop.Result, error) {
	e := messaging.ExecutionOf(ctx)
	a.mu.Lock()
	if e != nil {
		a.seen = append(a.seen, fmt.Sprintf("%s:%s:%s:%s", e.ID, e.Kind, opts.InputKind, input))
	} else {
		a.seen = append(a.seen, "no execution:"+input)
	}
	a.mu.Unlock()
	if input == "block" {
		select {
		case <-a.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if input == "act" && e != nil {
		e.Act()
	}
	tip, ok := a.store.Tip(session)
	if !ok {
		return nil, loop.ErrNoSession
	}
	return &loop.Result{Text: "re: " + input, Session: tip}, nil
}

func (a *channelAgent) turns() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func channelServer(t *testing.T, state string, enforce bool) (*Server, *channelAgent, *httptest.Server) {
	t.Helper()
	store, err := sessions.Open(filepath.Join(state, "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := &channelAgent{store: store, release: make(chan struct{})}
	s, err := New(Options{Key: key, ChannelKey: coreKey, ChannelEnforce: enforce, Store: store, Agent: ag, StateDir: filepath.Join(state, "state")})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, ag, srv
}

var nonceSeq int

// signed sends a channel request signed with k (now: its time).
func signed(t *testing.T, srv *httptest.Server, k, method, path string, body any, now time.Time) (int, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	nonceSeq++
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+key)
	if k != "" {
		req.Header.Set(wire.ChannelSignatureHeader, wire.SignChannel(k, method, path, raw, now.Unix(), fmt.Sprintf("n%d-%d", nonceSeq, now.UnixNano())))
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func status(t *testing.T, raw []byte) wire.ChannelTurnStatus {
	t.Helper()
	var s wire.ChannelTurnStatus
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return s
}

// A channel turn is admitted once, runs once, and its result is kept until the core takes it.
func TestChannelTurnAdmittedOnceAndKept(t *testing.T) {
	_, ag, srv := channelServer(t, t.TempDir(), false)
	turn := wire.ChannelTurn{Execution: "exec-0001", Kind: messaging.ExecInbound, Session: "whatsapp-0a1b2c", Title: "Spring promo", Message: "hola"}
	code, raw := signed(t, srv, coreKey, "POST", "/v1/channel/turns", turn, time.Now())
	if code != http.StatusAccepted || status(t, raw).State != wire.ChannelRunning {
		t.Fatalf("%d %s", code, raw)
	}
	code, raw = signed(t, srv, coreKey, "GET", "/v1/channel/turns/exec-0001?wait=5", nil, time.Now())
	st := status(t, raw)
	if code != http.StatusOK || st.State != wire.ChannelOK || st.Text != "re: hola" || st.Session == "" {
		t.Fatalf("%d %+v", code, st)
	}
	// the same request again (a lost 202, a core restart): the same turn, not a second one
	code, raw = signed(t, srv, coreKey, "POST", "/v1/channel/turns", turn, time.Now())
	if code != http.StatusAccepted || status(t, raw).State != wire.ChannelOK || len(ag.turns()) != 1 {
		t.Fatalf("a repeat ran again: %d %s %v", code, raw, ag.turns())
	}
	other := turn
	other.Message = "something else"
	if code, _ := signed(t, srv, coreKey, "POST", "/v1/channel/turns", other, time.Now()); code != http.StatusConflict {
		t.Fatalf("another request under the same execution: %d", code)
	}
	if code, _ := signed(t, srv, coreKey, "DELETE", "/v1/channel/turns/exec-0001", nil, time.Now()); code != http.StatusOK {
		t.Fatalf("take: %d", code)
	}
	if code, _ := signed(t, srv, coreKey, "GET", "/v1/channel/turns/exec-0001", nil, time.Now()); code != http.StatusNotFound {
		t.Fatalf("taken, still there: %d", code)
	}
}

// Only the core's signature, fresh and once, runs a channel turn, and only in a channel conversation.
func TestChannelTurnSignatureAndScope(t *testing.T) {
	_, ag, srv := channelServer(t, t.TempDir(), false)
	turn := wire.ChannelTurn{Execution: "exec-0002", Kind: messaging.ExecInbound, Session: "whatsapp-ffee", Message: "hola"}
	for name, k := range map[string]string{"unsigned": "", "wrong key": "another-key-0123456789"} {
		if code, _ := signed(t, srv, k, "POST", "/v1/channel/turns", turn, time.Now()); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, code)
		}
	}
	if code, _ := signed(t, srv, coreKey, "POST", "/v1/channel/turns", turn, time.Now().Add(-time.Hour)); code != http.StatusUnauthorized {
		t.Errorf("an old signature: %d", code)
	}
	raw, _ := json.Marshal(turn)
	header := wire.SignChannel(coreKey, "POST", "/v1/channel/turns", raw, time.Now().Unix(), "once-only-nonce")
	for i, want := range []int{http.StatusAccepted, http.StatusUnauthorized} {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/channel/turns", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set(wire.ChannelSignatureHeader, header)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("nonce use %d: %d, want %d", i+1, res.StatusCode, want)
		}
	}
	for _, sess := range []string{"slack-C1-1.2", "my-chat", "telegram-42"} {
		bad := turn
		bad.Execution, bad.Session = "exec-scope-"+fmt.Sprint(len(sess)), sess
		if code, _ := signed(t, srv, coreKey, "POST", "/v1/channel/turns", bad, time.Now()); code != http.StatusForbidden {
			t.Errorf("session %s: %d", sess, code)
		}
	}
	waitFor(t, func() bool { return len(ag.turns()) == 1 })

	none, _, nsrv := newServer(t, t.TempDir())
	_ = none
	if code, _ := signed(t, nsrv, coreKey, "POST", "/v1/channel/turns", turn, time.Now()); code != http.StatusNotImplemented {
		t.Errorf("no core key: %d", code)
	}
}

// A busy conversation admits nothing; a restart interrupts what ran; an unreadable file stops the start.
func TestChannelTurnBusyRestartAndFile(t *testing.T) {
	dir := t.TempDir()
	s, ag, srv := channelServer(t, dir, false)
	first := wire.ChannelTurn{Execution: "exec-busy-1", Kind: messaging.ExecInbound, Session: "whatsapp-b1", Message: "block"}
	if code, _ := signed(t, srv, coreKey, "POST", "/v1/channel/turns", first, time.Now()); code != http.StatusAccepted {
		t.Fatalf("%d", code)
	}
	waitFor(t, func() bool { return len(ag.turns()) == 1 })
	second := wire.ChannelTurn{Execution: "exec-busy-2", Kind: messaging.ExecReturn, Session: "whatsapp-b1", Message: "later"}
	if code, _ := signed(t, srv, coreKey, "POST", "/v1/channel/turns", second, time.Now()); code != http.StatusConflict {
		t.Fatalf("busy: %d", code)
	}
	if _, ok := s.ChannelTurn("exec-busy-2"); ok {
		t.Fatal("a refused turn was admitted")
	}
	// a restart while exec-busy-1 runs: it is interrupted, never run again
	store, _ := sessions.Open(filepath.Join(dir, "sessions"), nil)
	again, err := New(Options{Key: key, ChannelKey: coreKey, Store: store, Agent: ag, StateDir: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := again.ChannelTurn("exec-busy-1"); !ok || st.State != wire.ChannelInterrupted {
		t.Fatalf("after a restart: %+v %v", st, ok)
	}
	close(ag.release)
	bad := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bad, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "state", channelFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Key: key, Store: store, Agent: ag, StateDir: filepath.Join(bad, "state")}); err == nil {
		t.Fatal("an unreadable channel-turns file did not stop the start")
	}
}

// The turn runs with its execution; a return is marked so; what it did is counted.
func TestChannelTurnExecution(t *testing.T) {
	_, ag, srv := channelServer(t, t.TempDir(), false)
	turn := wire.ChannelTurn{Execution: "exec-ret-01", Kind: messaging.ExecReturn, Session: "whatsapp-r1", Message: "act"}
	if code, _ := signed(t, srv, coreKey, "POST", "/v1/channel/turns", turn, time.Now()); code != http.StatusAccepted {
		t.Fatalf("%d", code)
	}
	_, raw := signed(t, srv, coreKey, "GET", "/v1/channel/turns/exec-ret-01?wait=5", nil, time.Now())
	if st := status(t, raw); st.State != wire.ChannelOK || st.Acted != 1 {
		t.Fatalf("%+v", st)
	}
	if got := ag.turns(); len(got) != 1 || got[0] != "exec-ret-01:return:"+wire.KindReturn+":act" {
		t.Fatalf("the turn saw %v", got)
	}
}

// With enforcement, the core's channel conversations run only as channel turns.
func TestChannelEnforcement(t *testing.T) {
	for _, enforce := range []bool{false, true} {
		s, _, srv := channelServer(t, t.TempDir(), enforce)
		if _, err := s.store.Create(wire.Session{ID: "whatsapp-e1", Source: SourceAPI}); err != nil {
			t.Fatal(err)
		}
		c := call(srv, key)
		want := http.StatusOK
		if enforce {
			want = http.StatusForbidden
		}
		if code, raw, _ := c(context.Background(), "POST", "/v1/sessions/whatsapp-e1/chat", map[string]string{"message": "hola"}); code != want {
			t.Errorf("enforce=%v chat: %d %s", enforce, code, raw)
		}
		wantRun := http.StatusAccepted
		if enforce {
			wantRun = http.StatusForbidden
		}
		if code, _, _ := c(context.Background(), "POST", "/v1/runs", map[string]string{"input": "x", "session_id": "WhatsApp-e2"}); code != wantRun {
			t.Errorf("enforce=%v runs: %d", enforce, code)
		}
		code, raw, _ := c(context.Background(), "GET", "/v1/capabilities", nil)
		var caps wire.Capabilities
		_ = json.Unmarshal(raw, &caps)
		if code != http.StatusOK || caps.ChannelTurns != wire.ChannelTurnsV1 || caps.Enforcing != enforce {
			t.Errorf("capabilities %d %+v", code, caps)
		}
		// channel turns still run them
		if code, _ := signed(t, srv, coreKey, "POST", "/v1/channel/turns", wire.ChannelTurn{Execution: "exec-enf-01", Kind: "inbound", Session: "whatsapp-e1", Message: "hola"}, time.Now()); code != http.StatusAccepted {
			t.Errorf("enforce=%v channel turn: %d", enforce, code)
		}
	}
}
