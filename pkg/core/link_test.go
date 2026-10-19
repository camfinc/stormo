package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/link"
)

// fakeAgentAPI is an agent's engine API: activity reads, and wakes recorded.
type fakeAgentAPI struct {
	mu    sync.Mutex
	wakes []string
}

func (f *fakeAgentAPI) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health/detailed":
			fmt.Fprint(w, `{"active_agents":1,"gateway_busy":false,"platforms":{}}`)
		case "/api/sessions":
			fmt.Fprint(w, `{"data":[{"source":"slack","last_active":1791337630}]}`)
		case "/api/jobs":
			fmt.Fprint(w, `{"jobs":[]}`)
		case "/v1/runs":
			var b struct {
				Input string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			f.mu.Lock()
			f.wakes = append(f.wakes, b.Input)
			f.mu.Unlock()
			w.WriteHeader(202)
			fmt.Fprint(w, `{"run_id":"r1"}`)
		default:
			w.WriteHeader(404)
		}
	})
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestAgentLinkStatusCommandsAndBuffering(t *testing.T) {
	_, inst := fixture(t)
	db := testDB(t)
	keys := testKeys(t)
	port := freePort(t)
	start := func() *Core {
		t.Helper()
		c, err := StartCore(CoreOptions{Inst: inst, Port: port, NoFleet: true, DB: db, Keys: keys, OwnerToken: "owner-token-0123456789", NoMirror: true,
			Roster: func() []BusAgent { return []BusAgent{{ID: "atlas", Unit: "sales"}, {ID: "nova", Unit: "media"}} }})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := start()

	api := &fakeAgentAPI{}
	agentAPI := httptest.NewServer(api.handler())
	defer agentAPI.Close()
	naps := make(chan string, 4)
	relayPort := freePort(t)
	ag := link.NewAgent(link.AgentOptions{
		Agent: "atlas", Placement: "vps", Engine: "hermes", Version: "test",
		CoreURL: fmt.Sprintf("http://127.0.0.1:%d", port), CoreKey: atlasKey,
		AgentAPI: agentAPI.URL, APIKey: "atlas-api-key-0123456789", Runtime: hermes.NewRuntime(),
		Spool: &link.Spool{Dir: t.TempDir()}, Listen: fmt.Sprintf("127.0.0.1:%d", relayPort), StatusEvery: 100 * time.Millisecond,
		Nap:  func(reason string) error { naps <- reason; return nil },
		Logf: t.Logf,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)

	// Connected, with the agent's activity as its sidecar reads it.
	eventually(t, "the link", func() bool { return c.Links.Connected("atlas") })
	eventually(t, "status", func() bool {
		s := c.Links.Status("atlas")
		return s != nil && s.State == "running" && s.Activity != nil
	})
	if i := c.Links.Info("atlas"); i == nil || i.Placement != "vps" {
		t.Errorf("info: %+v", i)
	}

	// Commands come back with their answers.
	if err := c.Links.Command(ctx, "atlas", link.TypeWake, link.Wake{Input: "you have mail", Session: "swarm-bus-1", Idempotency: "k1"}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	if len(api.wakes) != 1 || api.wakes[0] != "you have mail" {
		t.Errorf("wakes: %v", api.wakes)
	}
	api.mu.Unlock()
	if err := c.Links.Command(ctx, "atlas", link.TypeNap, link.Nap{Reason: "learn"}); err != nil || <-naps != "learn" {
		t.Errorf("nap: %v", err)
	}
	if err := c.Links.Command(ctx, "nova", link.TypeNap, nil); err != ErrNoLink {
		t.Errorf("nova has no link: %v", err)
	}

	// The relay passes hooks to the core.
	relay := fmt.Sprintf("http://127.0.0.1:%d", relayPort)
	deliver := func(tool string) int {
		body := hook("pre_tool_call", tool, map[string]any{}, map[string]any{"tool_call_id": tool}, time.Now())
		req, _ := http.NewRequest("POST", relay+"/ingest/hermes", bytes.NewReader(body))
		req.Header.Set("X-Hermes-Signature-256", sign(atlasKey, body))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if code := deliver("first_tool"); code < 200 || code > 299 {
		t.Errorf("relayed hook: %d", code)
	}
	if l := c.Monitor.Live("atlas"); l == nil || l.Tool != "first_tool" {
		t.Errorf("live after relayed hook: %+v", l)
	}

	// The core goes away: hooks are held, msg_send is queued, other MCP calls fail fast.
	c.Close()
	eventually(t, "the link to drop", func() bool { return !ag.Connected() })
	if code := deliver("held_tool"); code != 202 {
		t.Errorf("held hook: %d", code)
	}
	mcp := func(body string) string {
		r, err := http.Post(relay+"/mcp", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return string(b)
	}
	if out := mcp(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"msg_send","arguments":{"to":"nova","subject":"hi","body":"later"}}}`); !strings.Contains(out, "Queued") {
		t.Errorf("msg_send while away: %s", out)
	}
	if out := mcp(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"fs_ls","arguments":{}}}`); !strings.Contains(out, "unreachable") {
		t.Errorf("fs_ls while away: %s", out)
	}

	// Back: the link returns and what was held is delivered.
	c = start()
	defer c.Close()
	eventually(t, "the link again", func() bool { return c.Links.Connected("atlas") })
	eventually(t, "the held hook", func() bool { l := c.Monitor.Live("atlas"); return l != nil && l.Tool == "held_tool" })
	eventually(t, "the queued message", func() bool {
		ms, err := c.Bus.Inbox("nova", true, 10)
		return err == nil && len(ms) == 1
	})
}
