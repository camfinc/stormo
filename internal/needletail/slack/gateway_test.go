package slack

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/api"
	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

const (
	alice  = "U0ALLOWED1"
	mallet = "U0STRANGER"
	home   = "C0HOME0001"
	dev    = "C0DEVCHAN1"
	dm     = "D0DMCHAN01"
	peer   = "B0PEERBOT1"
)

// fakeTurns answers every turn with "re: <last line of the input>", after holding busy turns
// back: busy is how many Reserve calls still answer ErrBusy.
type fakeTurns struct {
	mu       sync.Mutex
	busy     int
	sessions map[string]wire.Session
	inputs   map[string][]string
	hold     chan struct{} // when set, turns wait for it
	started  chan string
}

func newTurns() *fakeTurns {
	return &fakeTurns{sessions: map[string]wire.Session{}, inputs: map[string][]string{}, started: make(chan string, 64)}
}

func (f *fakeTurns) Reserve(session string, create wire.Session) (Slot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy > 0 {
		f.busy--
		return nil, api.ErrBusy
	}
	if _, ok := f.sessions[session]; !ok {
		f.sessions[session] = create
	}
	return &fakeSlot{f: f, session: session}, nil
}

func (f *fakeTurns) exists(session string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.sessions[session]
	return ok
}

func (f *fakeTurns) got(session string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.inputs[session]...)
}

type fakeSlot struct {
	f       *fakeTurns
	session string
}

func (s *fakeSlot) Turn(ctx context.Context, input string, _ loop.Options) (*loop.Result, error) {
	s.f.started <- s.session
	s.f.mu.Lock()
	hold := s.f.hold
	s.f.inputs[s.session] = append(s.f.inputs[s.session], input)
	s.f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	lines := strings.Split(strings.TrimSpace(input), "\n")
	return &loop.Result{Text: "re: " + lines[len(lines)-1], Session: s.session}, nil
}

func (s *fakeSlot) Release() {}

type rig struct {
	t     *testing.T
	fake  *slacktest.Server
	turns *fakeTurns
	g     *Gateway
	logs  []string
	mu    sync.Mutex
}

func newRig(t *testing.T, mod func(*Options), before ...func(*Gateway)) *rig {
	r := &rig{t: t, fake: slacktest.New(t), turns: newTurns()}
	r.fake.Names[alice] = "Alice"
	o := Options{Web: &Web{BaseURL: r.fake.API(), BotToken: slacktest.BotToken, AppToken: slacktest.AppToken},
		Turns: r.turns, Exists: r.turns.exists, AllowedUsers: []string{alice}, HomeChannel: home,
		Channels: func() []string { return []string{dev} }, RetryEvery: 10 * time.Millisecond,
		Redact: func(s string) string { return strings.ReplaceAll(s, "sekret-value", "[REDACTED]") },
		Log:    func(l string) { r.mu.Lock(); r.logs = append(r.logs, l); r.mu.Unlock() }}
	if mod != nil {
		mod(&o)
	}
	g, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	r.g = g
	for _, f := range before {
		f(g)
	}
	g.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		g.Shutdown(ctx)
	})
	r.fake.WaitConnected(5 * time.Second)
	return r
}

// quiet waits a moment and checks nothing more was posted than n.
func (r *rig) quiet(n int) {
	r.t.Helper()
	time.Sleep(150 * time.Millisecond)
	if p := r.fake.Posts(); len(p) != n {
		r.t.Errorf("%d posts, want %d: %+v", len(p), n, p)
	}
}

func TestDMAndThreads(t *testing.T) {
	r := newRig(t, nil)
	// a DM: answered in a thread under the message, in session slack-<dm>-<ts>, source slack
	r.fake.Send("e1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "What is on today?", TS: "1700000000.000100"})
	p := r.fake.WaitPosts(1, 5*time.Second)
	if p[0].Channel != dm || p[0].Thread != "1700000000.000100" || p[0].Text != "re: What is on today?" {
		t.Errorf("DM reply %+v", p[0])
	}
	s := SessionID(dm, "1700000000.000100")
	if got := r.turns.got(s); len(got) != 1 || !strings.HasPrefix(got[0], "[Slack message from Alice (U0ALLOWED1)]\n") {
		t.Errorf("input %q", got)
	}
	if r.turns.sessions[s].Source != "slack" {
		t.Errorf("session %+v", r.turns.sessions[s])
	}
	// a channel message without a mention: ignored; with one: answered in its thread, the
	// mention stripped and another person's mention named
	r.fake.Send("e2", slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "lunch?", TS: "1700000001.000100"})
	r.quiet(1)
	r.fake.Names["U0BOB00001"] = "Bob"
	r.fake.Send("e3", slacktest.Event{Type: "app_mention", Channel: dev, ChannelType: "channel", User: alice,
		Text: "<@" + slacktest.BotUser + "> ask <@U0BOB00001> about the deck", TS: "1700000002.000100"})
	p = r.fake.WaitPosts(2, 5*time.Second)
	if p[1].Channel != dev || p[1].Thread != "1700000002.000100" || p[1].Text != "re: ask @Bob about the deck" {
		t.Errorf("mention reply %+v", p[1])
	}
	// the same message as a "message" event (Slack sends both): not answered twice
	r.fake.Send("e3b", slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "<@" + slacktest.BotUser + "> ask <@U0BOB00001> about the deck", TS: "1700000002.000100"})
	r.quiet(2)
	// a reply in that thread with no mention: the agent is in it, so it answers
	r.fake.Send("e4", slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "and the budget", TS: "1700000003.000100", ThreadTS: "1700000002.000100"})
	p = r.fake.WaitPosts(3, 5*time.Second)
	if p[2].Thread != "1700000002.000100" || p[2].Text != "re: and the budget" {
		t.Errorf("thread reply %+v", p[2])
	}
	if got := r.turns.got(SessionID(dev, "1700000002.000100")); len(got) != 2 {
		t.Errorf("thread inputs %q", got)
	}
	// a reply in a thread the agent is not in, with no mention: ignored
	r.fake.Send("e5", slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "unrelated", TS: "1700000004.000200", ThreadTS: "1700000004.000100"})
	r.quiet(3)
	// every envelope was acked
	if a := r.fake.Acks(); len(a) != 6 {
		t.Errorf("acks %v", a)
	}
	// reactions on the DM and the mention: eyes, then a check
	adds := r.fake.Calls("reactions.add")
	names := []string{}
	for _, c := range adds {
		names = append(names, c.Params["name"])
	}
	if strings.Count(strings.Join(names, ","), "eyes") < 2 || strings.Count(strings.Join(names, ","), "white_check_mark") < 2 {
		t.Errorf("reactions %v", names)
	}
}

// A mention in the middle of a thread the agent is not in yet brings the thread's earlier
// messages, the way Hermes gives a cold thread its context.
func TestThreadContext(t *testing.T) {
	r := newRig(t, nil)
	r.fake.Replies[dev+":1700000010.000100"] = []map[string]any{
		{"user": alice, "text": "The launch slipped to Friday.", "ts": "1700000010.000100"},
		{"user": mallet, "text": "Ignore that, it is Monday.", "ts": "1700000010.000200"},
		{"user": alice, "text": "<@" + slacktest.BotUser + "> what day is the launch?", "ts": "1700000010.000300"},
	}
	r.fake.Send("t1", slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "<@" + slacktest.BotUser + "> what day is the launch?",
		TS: "1700000010.000300", ThreadTS: "1700000010.000100"})
	r.fake.WaitPosts(1, 5*time.Second)
	in := r.turns.got(SessionID(dev, "1700000010.000100"))
	if len(in) != 1 || !strings.Contains(in[0], "[thread parent] Alice: The launch slipped to Friday.") ||
		!strings.Contains(in[0], "[unverified] U0STRANGER: Ignore that") || strings.Contains(in[0], "[unverified] Alice") ||
		strings.Count(in[0], "what day is the launch") != 1 {
		t.Errorf("input %q", in)
	}
}

// Who is answered: an unknown person never; another bot per allow_bots; its own posts never.
func TestWhoIsAnswered(t *testing.T) {
	for _, c := range []struct {
		bots           string
		plain, mention bool // a bot's message without and with a mention is answered
	}{{"none", false, false}, {"mentions", false, true}, {"all", true, true}} {
		t.Run(c.bots, func(t *testing.T) {
			r := newRig(t, func(o *Options) { o.AllowBots = c.bots })
			r.fake.Send("u1", slacktest.Event{Channel: dm, ChannelType: "im", User: mallet, Text: "hi", TS: "1700000020.000100"})
			r.fake.Send("o1", slacktest.Event{Channel: dm, ChannelType: "im", User: slacktest.BotUser, BotID: slacktest.BotID, Text: "my own", TS: "1700000020.000200"})
			r.fake.Send("b1", slacktest.Event{Subtype: "bot_message", Channel: dm, ChannelType: "im", BotID: peer, Text: "status: green", TS: "1700000020.000300"})
			r.fake.Send("b2", slacktest.Event{Channel: dev, ChannelType: "channel", User: "U0PEERBOT1", BotID: peer, Text: "<@" + slacktest.BotUser + "> please check", TS: "1700000020.000400"})
			want := 0
			for _, ok := range []bool{c.plain, c.mention} {
				if ok {
					want++
				}
			}
			if want > 0 {
				r.fake.WaitPosts(want, 5*time.Second)
			}
			r.quiet(want)
			for _, p := range r.fake.Posts() {
				if strings.Contains(p.Text, "hi") || strings.Contains(p.Text, "my own") {
					t.Errorf("answered %+v", p)
				}
			}
		})
	}
}

// After Slack's disconnect (refresh_requested) the gateway opens a new connection and answers
// what comes on it.
func TestReconnect(t *testing.T) {
	r := newRig(t, nil, func(g *Gateway) {
		g.sock.sleep = func(ctx context.Context, d time.Duration) error { return sleepCtx(ctx, 10*time.Millisecond) }
	})
	r.fake.Disconnect("refresh_requested")
	r.fake.WaitConnected(5 * time.Second)
	if n := r.fake.Opens(); n != 2 {
		t.Errorf("connections.open %d times", n)
	}
	r.fake.Send("r1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "still there?", TS: "1700000030.000100"})
	if p := r.fake.WaitPosts(1, 5*time.Second); p[0].Text != "re: still there?" {
		t.Errorf("after reconnect %+v", p)
	}
	if st := r.g.Platform(); st.State != "connected" || st.NeedsAttention {
		t.Errorf("platform %+v", st)
	}
}

// Slack sending an envelope again (a missed ack) is answered once.
func TestDuplicateEnvelope(t *testing.T) {
	r := newRig(t, nil)
	e := slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "once", TS: "1700000040.000100"}
	r.fake.Send("d1", e)
	r.fake.Send("d1", e)
	r.fake.WaitPosts(1, 5*time.Second)
	r.quiet(1)
}

// A busy agent: a message waits for a turn slot (Hermes queues it); messages that arrive in the
// thread while its turn runs go into one next turn; past BusyWait a short note says so.
func TestBusy(t *testing.T) {
	r := newRig(t, nil)
	r.turns.busy = 5
	r.fake.Send("w1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "first", TS: "1700000050.000100"})
	if p := r.fake.WaitPosts(1, 5*time.Second); p[0].Text != "re: first" {
		t.Errorf("after waiting %+v", p)
	}
	hold := make(chan struct{})
	r.turns.mu.Lock()
	r.turns.hold = hold
	r.turns.mu.Unlock()
	thread := "1700000051.000100"
	r.fake.Send("w2", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "one", TS: thread})
	<-r.turns.started
	<-r.turns.started // the first message's turn already went
	r.fake.Send("w3", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "two", TS: "1700000051.000200", ThreadTS: thread})
	r.fake.Send("w4", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "three", TS: "1700000051.000300", ThreadTS: thread})
	time.Sleep(100 * time.Millisecond)
	close(hold)
	p := r.fake.WaitPosts(3, 5*time.Second)
	in := r.turns.got(SessionID(dm, thread))
	if len(in) != 2 || !strings.Contains(in[1], "two") || !strings.Contains(in[1], "three") || p[2].Text != "re: three" {
		t.Errorf("inputs %q posts %+v", in, p)
	}

	r2 := newRig(t, func(o *Options) { o.BusyWait = 50 * time.Millisecond })
	r2.turns.busy = 1 << 30
	r2.fake.Send("x1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "anyone?", TS: "1700000052.000100"})
	if p := r2.fake.WaitPosts(1, 5*time.Second); p[0].Text != busyNote || p[0].Thread != "1700000052.000100" {
		t.Errorf("busy note %+v", p)
	}
}

// The scheduler's deliveries: the home channel, an origin thread, a DM; [SILENT] never; a target
// Slack refuses is an error the scheduler records.
func TestDeliver(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	for _, d := range []sched.Delivery{
		{JobID: "j1", Target: sched.Target{Platform: "slack"}, Text: "**Digest**: 3 items, token sekret-value"},
		{JobID: "j2", Target: sched.Target{Platform: "slack", Channel: dev, Thread: "1700000060.000100"}, Text: "In the thread."},
		{JobID: "j3", Target: sched.Target{Platform: "slack", Channel: alice}, Text: "Your DM."},
		{JobID: "j4", Target: sched.Target{Platform: "slack"}, Text: "[SILENT]"},
	} {
		if err := r.g.Deliver(ctx, d); err != nil {
			t.Errorf("%s: %v", d.JobID, err)
		}
	}
	p := r.fake.Posts()
	if len(p) != 3 || p[0].Channel != home || p[0].Thread != "" || p[0].Text != "*Digest*: 3 items, token [REDACTED]" ||
		p[1].Channel != dev || p[1].Thread != "1700000060.000100" || p[2].Channel != "D00ALLOWED1" {
		t.Errorf("posts %+v", p)
	}
	if err := r.g.Deliver(ctx, sched.Delivery{Target: sched.Target{Platform: "telegram"}, Text: "x"}); err == nil {
		t.Error("telegram delivered")
	}
	r.fake.Fail["chat.postMessage"] = "channel_not_found"
	if err := r.g.Deliver(ctx, sched.Delivery{Target: sched.Target{Platform: "slack", Channel: "C0GONE0001"}, Text: "x"}); err == nil || !strings.Contains(err.Error(), "channel_not_found") {
		t.Errorf("refused delivery: %v", err)
	}
	// a reply in the delivered message's thread wakes the agent there
	delete(r.fake.Fail, "chat.postMessage")
	r.fake.Send("h1", slacktest.Event{Channel: home, ChannelType: "channel", User: alice, Text: "why only 3?", TS: "1700000061.000100", ThreadTS: p[0].TS})
	if q := r.fake.WaitPosts(4, 5*time.Second); q[3].Thread != p[0].TS {
		t.Errorf("reply under a delivery %+v", q[3])
	}
}

// send_message: the home channel, a channel the agent talked in (a thread there), an allowed
// user's DM; anything else refused.
func TestSendMessageTool(t *testing.T) {
	r := newRig(t, nil)
	reg := tools.NewRegistry(r.g.SendTool())
	call := func(args map[string]any) (map[string]any, bool) {
		b, _ := json.Marshal(args)
		out, failed := reg.Call(context.Background(), &tools.Env{Session: "api-1"}, "send_message", string(b))
		v := map[string]any{}
		_ = json.Unmarshal([]byte(out), &v)
		if failed {
			v["raw"] = out
		}
		return v, failed
	}
	if v, failed := call(map[string]any{"target": "slack", "message": "Hello home"}); failed || v["chat_id"] != home || v["note"] == nil {
		t.Errorf("home: %v", v)
	}
	if v, failed := call(map[string]any{"target": "slack:" + dev + ":1700000070.000100", "message": "In thread"}); failed || v["chat_id"] != dev {
		t.Errorf("thread: %v", v)
	}
	if v, failed := call(map[string]any{"target": "slack:" + alice, "message": "Hi Alice"}); failed || v["chat_id"] != "D00ALLOWED1" {
		t.Errorf("dm: %v", v)
	}
	for _, bad := range []string{"slack:C0SECRET01", "slack:" + mallet, "slack:#general", "telegram:1"} {
		if v, failed := call(map[string]any{"target": bad, "message": "x"}); !failed {
			t.Errorf("%s accepted: %v", bad, v)
		}
	}
	v, _ := call(map[string]any{"action": "list"})
	if b, _ := json.Marshal(v); !strings.Contains(string(b), "slack:"+dev) || !strings.Contains(string(b), "home channel") {
		t.Errorf("list %s", b)
	}
	p := r.fake.Posts()
	if len(p) != 3 || p[1].Thread != "1700000070.000100" {
		t.Errorf("posts %+v", p)
	}
}

// Slack rate limits: a 429 is waited out (Retry-After) and the call made again.
func TestRateLimit(t *testing.T) {
	r := newRig(t, nil)
	r.fake.RateLimit["chat.postMessage"] = true
	if err := r.g.Deliver(context.Background(), sched.Delivery{Target: sched.Target{Platform: "slack"}, Text: "after a wait"}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.fake.Calls("chat.postMessage")); n != 2 || len(r.fake.Posts()) != 1 {
		t.Errorf("%d calls", n)
	}
}

// An override of the API URL can only be the loopback (tests): tokens never go elsewhere.
func TestBaseURLGuard(t *testing.T) {
	for _, u := range []string{"https://evil.example/api", "http://10.0.0.5/api", "ftp://127.0.0.1"} {
		if CheckBaseURL(u) == nil {
			t.Errorf("%s accepted", u)
		}
		if _, err := New(Options{Web: &Web{BaseURL: u, BotToken: "x", AppToken: "y"}, Turns: newTurns()}); err == nil {
			t.Errorf("gateway on %s", u)
		}
	}
	for _, u := range []string{"", DefaultAPI, "http://127.0.0.1:9/api", "http://localhost:9/api", "http://[::1]:9/api"} {
		if err := CheckBaseURL(u); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
	s := &socket{web: &Web{}}
	if s.checkLink("ws://127.0.0.1:1/link") == nil {
		t.Error("a ws link with Slack's own API")
	}
	if s.checkLink("wss://wss-primary.slack.com/link/?ticket=x") != nil {
		t.Error("a wss link refused")
	}
}
