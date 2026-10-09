package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type wakeCall struct{ agent, input, session, idem string }

type busFixture struct {
	bus    *Bus
	db     *DB
	mu     sync.Mutex
	agents map[string]*BusAgent
	wakes  []wakeCall
	mirror []string
	fail   error
	now    time.Time
}

func newBusFixture(t *testing.T, limits BusLimits) *busFixture {
	t.Helper()
	f := &busFixture{db: testDB(t), now: time.Now(), agents: map[string]*BusAgent{
		"atlas": {ID: "atlas", Unit: "sales", Running: true, Endpoint: "http://atlas"},
		"nova":  {ID: "nova", Unit: "media", Running: true, Endpoint: "http://nova"},
		"scout": {ID: "scout", Unit: "sales", Running: false, Endpoint: "http://scout"},
	}}
	f.bus = NewBus(BusOptions{DB: f.db, Limits: limits, Now: func() time.Time { return f.now },
		Roster: func() []BusAgent {
			f.mu.Lock()
			defer f.mu.Unlock()
			out := []BusAgent{}
			for _, a := range f.agents {
				out = append(out, *a)
			}
			return out
		},
		Wake: func(_ context.Context, a BusAgent, input, session, idem string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.fail != nil {
				return f.fail
			}
			f.wakes = append(f.wakes, wakeCall{a.ID, input, session, idem})
			return nil
		},
		Mirror: func(agent, text string) error { f.mirror = append(f.mirror, agent+": "+text); return nil },
	})
	return f
}

func (f *busFixture) takeWakes() []wakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.wakes
	f.wakes = nil
	return w
}

func mustSend(t *testing.T, f *busFixture, from string, req SendRequest) *SendResult {
	t.Helper()
	r, err := f.bus.Send(from, req)
	if err != nil {
		t.Fatalf("send %s → %s: %v", from, req.To, err)
	}
	return r
}

func findingRules(t *testing.T, db *DB) []string {
	t.Helper()
	fs, err := db.Findings(false, 50)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Agent+":"+f.Rule)
	}
	return out
}

// Phase 5's done-when: agent A asks agent B a question and gets an answer, no person in the loop.
func TestAskAndAnswerWithWakeRuns(t *testing.T) {
	f := newBusFixture(t, BusLimits{})
	q := mustSend(t, f, "atlas", SendRequest{To: "nova", Subject: "Banner sizes for the spring promo?", Body: "Which sizes do you need from sales by Friday?"})
	if q.Thread != q.ID || len(q.Recipients) != 1 || q.Recipients[0] != "nova" {
		t.Fatalf("send %+v", q)
	}
	f.bus.WakeRound()
	w := f.takeWakes()
	if len(w) != 1 || w[0].agent != "nova" || w[0].session != fmt.Sprintf("swarm-bus-%d", q.Thread) {
		t.Fatalf("wakes %+v", w)
	}
	if !strings.Contains(w[0].input, "[AGENT MESSAGE from atlas] Banner sizes") || !strings.Contains(w[0].input, fmt.Sprintf("msg_send(thread=%d)", q.Thread)) {
		t.Errorf("wake input %q", w[0].input)
	}
	if strings.Contains(w[0].input, "Friday") {
		t.Error("the wake input names the subject, never the body")
	}
	f.bus.WakeRound()
	if w := f.takeWakes(); len(w) != 0 {
		t.Fatalf("woken twice for one message: %+v", w)
	}

	in, _ := f.bus.Inbox("nova", true, 0)
	if len(in) != 1 || in[0].From != "atlas" || in[0].Read != "" {
		t.Fatalf("nova's inbox %+v", in)
	}
	m, err := f.bus.Read("nova", q.ID)
	if err != nil || m.Body != "Which sizes do you need from sales by Friday?" || m.Read == "" {
		t.Fatalf("read %+v %v", m, err)
	}
	if in, _ := f.bus.Inbox("nova", true, 0); len(in) != 0 {
		t.Errorf("read messages leave the unread inbox: %+v", in)
	}
	a := mustSend(t, f, "nova", SendRequest{To: "atlas", Subject: "Re: banner sizes", Body: "1200x628 and 1080x1080.", Thread: q.ID})
	if a.Thread != q.Thread {
		t.Errorf("reply thread %d, want %d", a.Thread, q.Thread)
	}
	f.bus.WakeRound()
	if w := f.takeWakes(); len(w) != 1 || w[0].agent != "atlas" {
		t.Fatalf("atlas is woken by the answer: %+v", w)
	}
	th, err := f.bus.Thread("atlas", q.ID)
	if err != nil || len(th) != 2 || th[1].Hop != 2 || th[1].Body != "1200x628 and 1080x1080." {
		t.Fatalf("thread %+v %v", th, err)
	}
	if err := f.bus.Ack("atlas", a.ID); err != nil {
		t.Fatal(err)
	}
	f.bus.WakeRound()
	if w := f.takeWakes(); len(w) != 0 {
		t.Errorf("acks wake no one: %+v", w)
	}
	sent, _ := f.bus.Read("atlas", q.ID)
	if len(sent.Recipients) != 1 || sent.Recipients[0].Read == "" || sent.Recipients[0].Woken == "" {
		t.Errorf("the sender sees delivery state: %+v", sent.Recipients)
	}
	if len(f.mirror) != 0 {
		t.Error("normal messages are not mirrored to Slack")
	}
}

func TestWakeWaitsForIdleAndRunningAgents(t *testing.T) {
	f := newBusFixture(t, BusLimits{WakeTries: 2})
	f.agents["nova"].Busy = true
	r := mustSend(t, f, "atlas", SendRequest{To: "unit:sales", Subject: "Pipeline review", Body: "Numbers are in."})
	if strings.Join(r.Recipients, ",") != "scout" || strings.Join(r.Offline, ",") != "scout" {
		t.Fatalf("unit:sales without the sender: %+v", r)
	}
	mustSend(t, f, "atlas", SendRequest{To: "nova", Subject: "Hi", Body: "busy?"})
	f.bus.WakeRound()
	if w := f.takeWakes(); len(w) != 0 {
		t.Fatalf("a busy or stopped agent is not woken: %+v", w)
	}
	f.agents["nova"].Busy = false
	f.bus.WakeRound()
	if w := f.takeWakes(); len(w) != 1 || w[0].agent != "nova" {
		t.Fatalf("woken once idle: %+v", w)
	}
	// Failed wakes are retried up to WakeTries, then the message waits for msg_inbox.
	mustSend(t, f, "atlas", SendRequest{To: "nova", Subject: "Again", Body: "x"})
	f.fail = errors.New("connection refused")
	f.bus.WakeRound()
	f.bus.WakeRound()
	f.fail = nil
	f.bus.WakeRound()
	if w := f.takeWakes(); len(w) != 0 {
		t.Errorf("gave up after 2 tries: %+v", w)
	}
	if in, _ := f.bus.Inbox("nova", true, 0); len(in) != 2 || in[0].Subject != "Again" {
		t.Errorf("unread until msg_read, woken or not: %+v", in)
	}
	// Group reaches everyone else, offline agents included.
	g := mustSend(t, f, "nova", SendRequest{To: "group", Subject: "Brand refresh", Body: "New palette in the shared space.", Priority: "urgent", Attach: []string{"/shared/group/brand/../brand/palette.md"}})
	if strings.Join(g.Recipients, ",") != "atlas,scout" {
		t.Errorf("group %+v", g)
	}
	if len(f.mirror) != 2 || !strings.HasPrefix(f.mirror[0], "atlas: Urgent message from nova") {
		t.Errorf("urgent is mirrored to each recipient's home channel: %v", f.mirror)
	}
	m, _ := f.bus.Read("atlas", g.ID)
	if len(m.Attach) != 1 || m.Attach[0] != "/shared/group/brand/palette.md" {
		t.Errorf("attachment %v", m.Attach)
	}
}

func TestSendRefusals(t *testing.T) {
	f := newBusFixture(t, BusLimits{PairPerHour: 3, MaxHops: 3, ThreadTTL: time.Hour, WakesPerHour: 1})
	bad := []struct {
		from string
		req  SendRequest
		want string
	}{
		{"atlas", SendRequest{To: "zed", Subject: "x", Body: "y"}, "unknown recipient"},
		{"atlas", SendRequest{To: "atlas", Subject: "x", Body: "y"}, "no one to send to"},
		{"atlas", SendRequest{To: "unit:legal", Subject: "x", Body: "y"}, "no agent in unit"},
		{"atlas", SendRequest{To: "nova", Subject: "", Body: "y"}, "subject"},
		{"atlas", SendRequest{To: "nova", Subject: "x", Body: strings.Repeat("y", maxBody+1)}, "body"},
		{"atlas", SendRequest{To: "nova", Subject: "x", Body: "y", Priority: "high"}, "priority"},
		{"atlas", SendRequest{To: "nova", Subject: "x", Body: "y", Attach: []string{"notes.md"}}, "absolute path"},
		{"atlas", SendRequest{To: "nova", Subject: "x", Body: "y", Attach: []string{"/opt/data/memories/MEMORY.md"}}, "only files in the shared space"},
		{"atlas", SendRequest{To: "nova", Subject: "x", Body: "y", Attach: []string{"/shared/media/brief.md"}}, "not yours"},
		{"atlas", SendRequest{To: "nova", Subject: "x", Body: "y", Attach: []string{"/shared/sales/../media/x.md"}}, "not yours"},
		{"atlas", SendRequest{To: "nova", Subject: "x", Body: "y", Attach: []string{"/shared/sales/clients.md"}}, "private to unit sales"},
		{"atlas", SendRequest{To: "nova", Subject: "x", Body: "y", Thread: 999}, "no message 999"},
		{"ghost", SendRequest{To: "nova", Subject: "x", Body: "y"}, "does not know"},
	}
	for _, c := range bad {
		_, err := f.bus.Send(c.from, c.req)
		var te ToolError
		if err == nil || !errors.As(err, &te) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: got %v, want a tool error with %q", c.req, err, c.want)
		}
	}
	// A unit's own layer may go to agents of that unit.
	mustSend(t, f, "atlas", SendRequest{To: "scout", Subject: "x", Body: "y", Attach: []string{"/shared/sales/clients.md"}})

	// Hops: a thread takes MaxHops messages.
	q := mustSend(t, f, "atlas", SendRequest{To: "nova", Subject: "ping", Body: "1"})
	mustSend(t, f, "nova", SendRequest{To: "atlas", Subject: "pong", Body: "2", Thread: q.ID})
	mustSend(t, f, "atlas", SendRequest{To: "nova", Subject: "ping", Body: "3", Thread: q.ID})
	if _, err := f.bus.Send("nova", SendRequest{To: "atlas", Subject: "pong", Body: "4", Thread: q.ID}); err == nil || !strings.Contains(err.Error(), "looping") {
		t.Errorf("hop limit: %v", err)
	}
	// Only parties to a thread reply in it or read it.
	if _, err := f.bus.Send("scout", SendRequest{To: "atlas", Subject: "me too", Body: "x", Thread: q.ID}); err == nil || !strings.Contains(err.Error(), "not one you are part of") {
		t.Errorf("outsider reply: %v", err)
	}
	if _, err := f.bus.Read("scout", q.ID); err == nil {
		t.Error("outsider read")
	}
	if _, err := f.bus.Thread("scout", q.ID); err == nil {
		t.Error("outsider thread")
	}
	if err := f.bus.Ack("scout", q.ID); err == nil {
		t.Error("outsider ack")
	}
	// Rate: atlas has sent nova 2 messages this hour; the 3rd is the last allowed.
	mustSend(t, f, "atlas", SendRequest{To: "nova", Subject: "one more", Body: "x"})
	if _, err := f.bus.Send("atlas", SendRequest{To: "nova", Subject: "too many", Body: "x"}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("pair rate: %v", err)
	}
	// TTL: an old thread takes no more messages.
	f.now = f.now.Add(2 * time.Hour)
	if _, err := f.bus.Send("nova", SendRequest{To: "scout", Subject: "late", Body: "x", Thread: q.ID}); err == nil || !strings.Contains(err.Error(), "older than") {
		t.Errorf("ttl: %v", err)
	}
	// Fleet wake cap: one wake an hour here.
	f.bus.WakeRound()
	f.bus.WakeRound()
	if w := f.takeWakes(); len(w) != 1 {
		t.Errorf("wake cap: %d wakes", len(w))
	}
	got := strings.Join(findingRules(t, f.db), ",")
	for _, want := range []string{"atlas:unit_boundary", "nova:bus_hops", "atlas:bus_rate", "nova:bus_thread_ttl", "core:bus_wake_cap"} {
		if !strings.Contains(got, want) {
			t.Errorf("finding %s missing from %s", want, got)
		}
	}
}

func TestCountsThreadsAndPrune(t *testing.T) {
	f := newBusFixture(t, BusLimits{})
	q := mustSend(t, f, "atlas", SendRequest{To: "nova", Subject: "Client X budget", Body: "secret numbers"})
	mustSend(t, f, "nova", SendRequest{To: "atlas", Subject: "Re", Body: "ok", Thread: q.ID})
	c, _ := f.bus.Counts()
	if c["nova"].Unread != 1 || c["nova"].Sent != 1 || c["atlas"].Sent != 1 || c["atlas"].Received != 1 {
		t.Errorf("counts %+v", c)
	}
	ths, _ := f.bus.Threads("", 10, false)
	if len(ths) != 1 || ths[0].Subject != "" || ths[0].Messages[0].Body != "" || len(ths[0].Messages[0].Recipients) != 1 {
		t.Errorf("metadata threads carry no subjects or bodies: %+v", ths)
	}
	ths, _ = f.bus.Threads("nova", 10, true)
	if len(ths) != 1 || ths[0].Subject != "Client X budget" || ths[0].Messages[0].Body != "secret numbers" {
		t.Errorf("owner threads %+v", ths)
	}
	if ths, _ := f.bus.Threads("scout", 10, true); len(ths) != 0 {
		t.Errorf("scout has no threads: %+v", ths)
	}
	f.now = f.now.Add(RetainMessages + time.Hour)
	if err := f.bus.Prune(RetainMessages); err != nil {
		t.Fatal(err)
	}
	if ths, _ := f.bus.Threads("", 10, true); len(ths) != 0 {
		t.Errorf("pruned: %+v", ths)
	}
	var n int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM deliveries`).Scan(&n)
	if n != 0 {
		t.Errorf("deliveries go with their messages: %d left", n)
	}
}

func rpc(t *testing.T, h http.HandlerFunc, key, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h(w, r)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestMCPOverHTTP(t *testing.T) {
	f := newBusFixture(t, BusLimits{})
	m := NewMCP(testKeys(t), BusTools(f.bus))
	h := m.Handle

	if code, _ := rpc(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); code != 401 {
		t.Errorf("no key: %d", code)
	}
	code, out := rpc(t, h, atlasKey, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"hermes"}}}`)
	res, _ := out["result"].(map[string]any)
	if code != 200 || res["protocolVersion"] != "2025-06-18" || res["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("initialize %d %v", code, out)
	}
	_, out = rpc(t, h, atlasKey, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2099-01-01"}}`)
	if out["result"].(map[string]any)["protocolVersion"] != mcpVersions[0] {
		t.Errorf("unknown version answers the newest: %v", out)
	}
	if code, _ := rpc(t, h, atlasKey, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Errorf("notification: %d", code)
	}
	_, out = rpc(t, h, atlasKey, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	names := []string{}
	for _, tl := range out["result"].(map[string]any)["tools"].([]any) {
		tool := tl.(map[string]any)
		names = append(names, tool["name"].(string))
		if tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("%s schema", tool["name"])
		}
	}
	if strings.Join(names, ",") != "msg_send,msg_inbox,msg_read,msg_ack,msg_thread" {
		t.Errorf("tools %v", names)
	}
	// The sender is whoever the key says, whatever the arguments claim.
	_, out = rpc(t, h, atlasKey, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"msg_send","arguments":{"to":"nova","subject":"Hello","body":"From atlas"}}}`)
	res = out["result"].(map[string]any)
	if res["isError"] != false || res["structuredContent"].(map[string]any)["thread"] == nil {
		t.Fatalf("msg_send %v", out)
	}
	_, out = rpc(t, h, novaKey, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"msg_inbox","arguments":{}}}`)
	text := out["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"from": "atlas"`) {
		t.Errorf("nova's inbox %s", text)
	}
	_, out = rpc(t, h, novaKey, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"msg_send","arguments":{"to":"atlas","subject":"x","body":"y","from":"atlas"}}}`)
	res = out["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "unknown field") {
		t.Errorf("unknown argument (a forged sender) is refused: %v", out)
	}
	_, out = rpc(t, h, novaKey, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"msg_read","arguments":{"id":424242}}}`)
	if out["result"].(map[string]any)["isError"] != true {
		t.Errorf("tool errors are results the model reads: %v", out)
	}
	_, out = rpc(t, h, novaKey, `{"jsonrpc":"2.0","id":8,"method":"nope"}`)
	if out["error"].(map[string]any)["code"].(float64) != -32601 {
		t.Errorf("unknown method %v", out)
	}
	r := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	r.Header.Set("Authorization", "Bearer "+atlasKey)
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != 405 {
		t.Errorf("GET: %d", w.Code)
	}
}

func TestMessagesAndFindingsNeedTheOwner(t *testing.T) {
	_, inst := fixture(t)
	f := newBusFixture(t, BusLimits{})
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: f.db, Keys: testKeys(t), OwnerToken: "owner-token-0123456789",
		Roster: f.bus.o.Roster, Wake: f.bus.o.Wake, NoMirror: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mustSend(t, f, "atlas", SendRequest{To: "nova", Subject: "Quarterly numbers", Body: "body text"})
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	get := func(p, auth string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, base+p, nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	for _, p := range []string{"/api/messages", "/api/findings"} {
		for _, who := range []string{"", atlasKey} {
			if code, _ := get(p, who); code != 401 {
				t.Errorf("%s as %q: %d", p, who, code)
			}
		}
	}
	if code, body := get("/api/messages?agent=nova", "owner-token-0123456789"); code != 200 || !strings.Contains(body, "body text") {
		t.Errorf("owner messages %d %s", code, body)
	}
	if code, body := get("/api/findings", "owner-token-0123456789"); code != 200 || !strings.Contains(body, `"findings":[]`) {
		t.Errorf("owner findings %d %s", code, body)
	}
	// MCP over the core's own listener, keyed.
	req, _ := http.NewRequest(http.MethodPost, base+"/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	req.Header.Set("Authorization", "Bearer "+novaKey)
	if r, err := http.DefaultClient.Do(req); err != nil || r.StatusCode != 200 {
		t.Errorf("mcp over HTTP: %v %v", r, err)
	}
}
