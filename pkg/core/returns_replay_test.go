package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/whatsapp"
	"go.mau.fi/whatsmeow/types"
)

// fakeChannel is an agent's channel turns: each turn runs a script once, when it is admitted
// (inbound: a person's message; return: an outcome brought back), its reply the script's. lose
// admits and runs that many Starts but answers them with a transport error; capsErr has the engine
// not answer its negotiation; hold keeps inbound turns running until released.
type fakeChannel struct {
	mu      sync.Mutex
	enforce bool
	capsErr bool
	lose    int
	hold    bool
	script  func(t engine.ChannelTurn) (string, error)
	turns   []engine.ChannelTurn
	status  map[string]engine.ChannelTurnStatus
	held    map[string]engine.ChannelTurnStatus
}

func (c *fakeChannel) Capabilities(context.Context) (engine.ChannelCaps, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capsErr {
		return engine.ChannelCaps{}, errors.New("connection refused")
	}
	return engine.ChannelCaps{Turns: true, Enforcing: c.enforce}, nil
}

func (c *fakeChannel) Start(_ context.Context, t engine.ChannelTurn) (engine.ChannelTurnStatus, error) {
	c.mu.Lock()
	if st, ok := c.status[t.Execution]; ok {
		c.mu.Unlock()
		return st, nil
	}
	if _, ok := c.held[t.Execution]; ok {
		c.mu.Unlock()
		return engine.ChannelTurnStatus{Execution: t.Execution, State: "running"}, nil
	}
	c.turns = append(c.turns, t)
	c.mu.Unlock()
	text, err := c.script(t) // outside the lock: a script may send over the bus
	st := engine.ChannelTurnStatus{Execution: t.Execution, State: "ok", Text: text, Tip: t.Session}
	if err != nil {
		st = engine.ChannelTurnStatus{Execution: t.Execution, State: "failed", Error: err.Error()}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hold && t.Kind == "inbound" {
		c.held[t.Execution] = st
	} else {
		c.status[t.Execution] = st
	}
	if c.lose > 0 {
		c.lose--
		return engine.ChannelTurnStatus{}, errors.New("POST /v1/channel/turns: connection reset by peer")
	}
	return engine.ChannelTurnStatus{Execution: t.Execution, State: "running"}, nil
}

func (c *fakeChannel) Status(_ context.Context, exec string, _ time.Duration) (engine.ChannelTurnStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.held[exec]; ok {
		return engine.ChannelTurnStatus{Execution: exec, State: "running"}, nil
	}
	st, ok := c.status[exec]
	if !ok {
		return engine.ChannelTurnStatus{}, engine.ErrChannelUnknown
	}
	return st, nil
}

func (c *fakeChannel) Take(context.Context, string) error { return nil }

// release ends the held turns.
func (c *fakeChannel) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, st := range c.held {
		c.status[id] = st
		delete(c.held, id)
	}
}

func (c *fakeChannel) kinds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []string{}
	for _, t := range c.turns {
		out = append(out, t.Kind)
	}
	return out
}

// fakePoster is the agent's WhatsApp, posting like the connector: the post rechecked before each
// part, the journal told before and after, a stop reported as a *whatsapp.PostError. refuse: the
// chat may not be posted to; uncertainAt: that part's send may or may not have gone; unrecordedAt:
// that part goes but its journal entry fails.
type fakePoster struct {
	mu           sync.Mutex
	posts        []string
	refuse       error
	uncertainAt  int
	unrecordedAt int
}

func newPoster() *fakePoster { return &fakePoster{uncertainAt: -1, unrecordedAt: -1} }

func (p *fakePoster) AllowedReply(whatsapp.Origin) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refuse
}

func (p *fakePoster) PostOrigin(ctx context.Context, o whatsapp.Origin, parts []string, from int, journal func(int, string, string) error) error {
	for i := from; i < len(parts); i++ {
		if err := p.AllowedReply(o); err != nil {
			return &whatsapp.PostError{Part: i, Unsent: true, Err: err}
		}
		if err := journal(i, "sending", ""); err != nil {
			return &whatsapp.PostError{Part: i, Unsent: true, Err: err}
		}
		p.mu.Lock()
		if i == p.uncertainAt {
			p.uncertainAt = -1
			p.mu.Unlock()
			return &whatsapp.PostError{Part: i, Err: context.DeadlineExceeded}
		}
		p.posts = append(p.posts, fmt.Sprintf("%s|%s|%s", o.Chat, o.MessageID, parts[i]))
		unrecorded := i == p.unrecordedAt
		if unrecorded {
			p.unrecordedAt = -1
		}
		p.mu.Unlock()
		if unrecorded {
			return &whatsapp.PostError{Part: i, Sent: true, ID: fmt.Sprintf("wa-%d", i), Err: errors.New("database is locked")}
		}
		if err := journal(i, "sent", fmt.Sprintf("wa-%d", i)); err != nil {
			return &whatsapp.PostError{Part: i, Sent: true, ID: fmt.Sprintf("wa-%d", i), Err: err}
		}
	}
	return nil
}

func (p *fakePoster) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.posts...)
}

type replay struct {
	f      *busFixture
	r      *Returns
	ch     *fakeChannel
	poster *fakePoster
	origin string
	optIn  bool
	runs   map[string]int // the scripts run, by kind
}

const sellersGroup = "120363012345678901@g.us"

// asked is the request the agent sends while answering the person, from that turn (its token).
func asked(f *busFixture, turn engine.ChannelTurn) error {
	_, err := f.bus.Send("atlas", SendRequest{To: "nova", Subject: "Spring promos for the sellers", Body: "List the agencies' spring promos.",
		Exec: turn.Execution, Op: turn.Execution + "/1", ExecToken: turn.Token})
	return err
}

func newReplay(t *testing.T, returnScript func(t engine.ChannelTurn) string) *replay {
	t.Helper()
	f := newBusFixture(t, BusLimits{})
	rp := &replay{f: f, poster: newPoster(), optIn: true, runs: map[string]int{}}
	rp.ch = &fakeChannel{enforce: true, status: map[string]engine.ChannelTurnStatus{}, held: map[string]engine.ChannelTurnStatus{}}
	var mu sync.Mutex
	rp.ch.script = func(turn engine.ChannelTurn) (string, error) {
		mu.Lock()
		rp.runs[turn.Kind]++
		mu.Unlock()
		if turn.Kind == "inbound" {
			if err := asked(f, turn); err != nil {
				return "", err
			}
			return "On it: I asked our research agent.", nil
		}
		return returnScript(turn), nil
	}
	rp.r = NewReturns(ReturnsOptions{DB: f.db, Bus: f.bus, Now: func() time.Time { return f.now },
		Settings: func(agent string) *manifest.Returns {
			if agent != "atlas" || !rp.optIn {
				return nil
			}
			return &manifest.Returns{UpdateAfter: 45 * time.Minute, EscalateAfter: 2 * time.Hour}
		},
		Channel: func(string) (engine.ChannelTurns, error) { return rp.ch, nil },
		Poster:  func(string) (ReturnPoster, error) { return rp.poster, nil }})
	rp.origin = rp.person(t, sellersGroup, "wa-in-1", "50760000000")
	ans, err := rp.r.InboundTurn(context.Background(), "atlas", "whatsapp-0a1b", whatsapp.Turn{Text: "spring promos?", Origin: rp.origin})
	if err != nil || ans.Text != "On it: I asked our research agent." {
		t.Fatalf("inbound %+v %v", ans, err)
	}
	return rp
}

// person records a person's message in chat as an origin.
func (rp *replay) person(t *testing.T, chat, msgID, number string) string {
	t.Helper()
	id, err := rp.r.RegisterOrigin("atlas", whatsapp.OriginEvent{Session: "whatsapp-" + msgID, Chat: chat, MessageID: msgID,
		Participant: number + "@s.whatsapp.net", Number: number, Name: "Seller " + number, Group: strings.HasSuffix(chat, "@g.us"),
		Verified: true, Quote: []byte{1, 2}, Account: "50760001111"})
	if err != nil || id == "" {
		t.Fatalf("origin %q %v", id, err)
	}
	return id
}

// execution is a turn of origin the core started, with its token.
func (rp *replay) execution(t *testing.T, origin string) engine.ChannelTurn {
	t.Helper()
	turn := engine.ChannelTurn{Execution: newID(), Token: newToken(), Kind: "inbound"}
	if _, err := rp.f.db.Exec(`INSERT INTO executions(id, agent, kind, origin, token, session, state, created) VALUES(?,?,?,?,?,?,?,0)`,
		turn.Execution, "atlas", "inbound", origin, turn.Token, "whatsapp-x", "running"); err != nil {
		t.Fatal(err)
	}
	return turn
}

func (rp *replay) request(t *testing.T) (id, thread int64) {
	t.Helper()
	if err := rp.f.db.QueryRow(`SELECT request, thread FROM returns WHERE agent = 'atlas' ORDER BY id LIMIT 1`).Scan(&id, &thread); err != nil {
		t.Fatalf("no return opened: %v", err)
	}
	return id, thread
}

func (rp *replay) rounds(n int) {
	for i := 0; i < n; i++ {
		rp.r.Round()
	}
}

func (rp *replay) state(t *testing.T) string {
	t.Helper()
	var s string
	_ = rp.f.db.QueryRow(`SELECT state FROM returns WHERE agent = 'atlas' ORDER BY id LIMIT 1`).Scan(&s)
	return s
}

func (rp *replay) answer(t *testing.T, req int64) *SendResult {
	t.Helper()
	return mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "Spring promos", Body: "Alpha 10%, Beta 15%, Gamma 2x1.", Thread: req, Answers: []int64{req}})
}

func (rp *replay) wokenAtlas() []wakeCall {
	var out []wakeCall
	for _, w := range rp.f.takeWakes() {
		if w.agent == "atlas" {
			out = append(out, w)
		}
	}
	return out
}

// The incident, end to end: a seller asks in a group, the agent delegates, the other agent answers;
// the answer brings the agent back into the seller's chat and its update is posted there, once,
// quoting what was asked, and the answer never also wakes it in the bus thread.
func TestReturnReplayTheIncident(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string {
		return "Three agencies have spring promos: Alpha 10%, Beta 15%, Gamma 2x1.\n[DONE]"
	})
	req, _ := rp.request(t)
	rp.f.bus.WakeRound()
	rp.f.takeWakes() // nova, for the request
	rp.f.now = rp.f.now.Add(10 * time.Minute)
	answer := rp.answer(t, req)
	rp.f.now = rp.f.now.Add(5 * time.Second)
	rp.f.bus.WakeRound()
	if w := rp.wokenAtlas(); len(w) != 0 {
		t.Fatalf("the answer also woke the agent in the bus thread: %+v", w)
	}
	rp.rounds(3)
	posts := rp.poster.sent()
	if len(posts) != 1 || posts[0] != sellersGroup+"|wa-in-1|Three agencies have spring promos: Alpha 10%, Beta 15%, Gamma 2x1." {
		t.Fatalf("posts %v", posts)
	}
	if got := rp.ch.kinds(); len(got) != 2 || got[1] != "return" || rp.ch.turns[1].Token == "" {
		t.Fatalf("turns %v", got)
	}
	cont := rp.ch.turns[1].In.Text
	if !strings.Contains(cont, "msg_read") || !strings.Contains(cont, fmt.Sprint(answer.ID)) || strings.Contains(cont, "Gamma") {
		t.Errorf("the continuation names the answer to read, never its words: %q", cont)
	}
	if s := rp.state(t); s != ReturnDone {
		t.Fatalf("state %s", s)
	}
	rp.rounds(3)
	if len(rp.poster.sent()) != 1 {
		t.Fatal("posted again")
	}
	rows, _ := rp.r.List("atlas", 0)
	if len(rows) != 1 || rows[0].Updates != 1 || rows[0].Fallback != 0 {
		t.Fatalf("listed %+v", rows)
	}
}

// No answer by the deadline: the agent is brought back to tell the person where it stands.
func TestReturnReplayDeadline(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Still checking with the agencies; more soon.\n[UPDATE]" })
	rp.request(t)
	rp.f.now = rp.f.now.Add(44 * time.Minute)
	rp.rounds(3)
	if len(rp.poster.sent()) != 0 {
		t.Fatal("an update before the deadline")
	}
	rp.f.now = rp.f.now.Add(2 * time.Minute)
	rp.rounds(3)
	if posts := rp.poster.sent(); len(posts) != 1 || !strings.HasSuffix(posts[0], "Still checking with the agencies; more soon.") {
		t.Fatalf("posts %v", posts)
	}
	if s := rp.state(t); s != ReturnUpdated {
		t.Fatalf("state %s", s)
	}
	rp.rounds(3)
	if len(rp.poster.sent()) != 1 {
		t.Fatal("updated again before the next deadline")
	}
}

// The person's chat cannot be reached: the update goes back to the bus, the agent told once in
// the request's thread by a core notice the wake loop delivers.
func TestReturnReplayFallback(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Here they are.\n[DONE]" })
	req, thread := rp.request(t)
	rp.f.bus.WakeRound()
	rp.f.takeWakes()
	rp.poster.refuse = fmt.Errorf("whatsapp: %s is no longer one of the allowed_groups", sellersGroup)
	rp.answer(t, req)
	rp.rounds(4)
	if len(rp.poster.sent()) != 0 {
		t.Fatal("posted to a chat it may not reach")
	}
	rp.f.now = rp.f.now.Add(5 * time.Second)
	rp.f.bus.WakeRound()
	fell := rp.wokenAtlas()
	if len(fell) != 1 || !strings.Contains(fell[0].input, "Tell the person yourself") || fell[0].session != fmt.Sprintf("swarm-bus-%d", thread) {
		t.Fatalf("fallback wakes %+v", fell)
	}
	ms, err := rp.f.bus.InboxIn("atlas", false, 10, "")
	if err != nil || len(ms) < 2 {
		t.Fatalf("the answer and the notice are the agent's to read now: %v %+v", err, ms)
	}
	rp.f.now = rp.f.now.Add(time.Minute)
	rp.f.bus.WakeRound()
	if w := rp.wokenAtlas(); len(w) != 0 {
		t.Fatalf("woken twice for the fallen-back update: %+v", w)
	}
	if got := findingRules(t, rp.f.db); !strings.Contains(strings.Join(got, ","), "atlas:return_undelivered") {
		t.Errorf("findings %v", got)
	}
}

// A send that may or may not have gone is held for a person: never posted again on its own.
func TestReturnReplayUnknownIsHeld(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Here they are.\n[DONE]" })
	req, _ := rp.request(t)
	rp.poster.uncertainAt = 0
	rp.answer(t, req)
	rp.rounds(4)
	if s := rp.state(t); s != ReturnUnknown {
		t.Fatalf("state %s", s)
	}
	rp.rounds(4)
	if len(rp.poster.sent()) != 0 {
		t.Fatal("an unknown send was retried on its own")
	}
	if got := findingRules(t, rp.f.db); !strings.Contains(strings.Join(got, ","), "atlas:return_unknown") {
		t.Errorf("findings %v", got)
	}
}

// One person's conversation per bus thread, for the thread's whole life: another person's request
// may not be sent into it, even after the first one closed.
func TestReturnOnePersonPerThread(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Done.\n[DONE]" })
	req, _ := rp.request(t)
	rp.answer(t, req)
	rp.rounds(4)
	if s := rp.state(t); s != ReturnDone {
		t.Fatalf("state %s", s)
	}
	other := rp.execution(t, rp.person(t, "50769999999@s.whatsapp.net", "wa-in-2", "50769999999"))
	if _, err := rp.f.bus.Send("atlas", SendRequest{To: "nova", Subject: "Also", Body: "y", Thread: req, Exec: other.Execution, Op: other.Execution + "/1",
		ExecToken: other.Token}); err == nil || !strings.Contains(err.Error(), "another person's conversation") {
		t.Fatalf("another person's request in a closed thread: %v", err)
	}
}

// One message may not answer requests made for two people.
func TestReturnAnswersForTwoPeopleRefused(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "[SILENT]" })
	first, _ := rp.request(t)
	other := rp.execution(t, rp.person(t, "50769999999@s.whatsapp.net", "wa-in-2", "50769999999"))
	if err := asked(rp.f, other); err != nil {
		t.Fatal(err)
	}
	var second int64
	_ = rp.f.db.QueryRow(`SELECT request FROM returns ORDER BY id DESC LIMIT 1`).Scan(&second)
	if second == first {
		t.Fatal("no second return")
	}
	if _, err := rp.f.bus.Send("nova", SendRequest{To: "atlas", Subject: "Both", Body: "z", Answers: []int64{first, second}}); err == nil ||
		!strings.Contains(err.Error(), "different people") {
		t.Fatalf("one answer for two people: %v", err)
	}
	if _, err := rp.f.bus.Send("nova", SendRequest{To: "atlas", Subject: "First", Body: "z", Thread: first, Answers: []int64{first}}); err != nil {
		t.Fatalf("an answer for one: %v", err)
	}
}

// A call names its turn only with that turn's token: a forged or unknown execution is refused,
// and a call with none opens no return.
func TestReturnExecutionNeedsItsToken(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "[SILENT]" })
	turn := rp.execution(t, rp.origin)
	for _, bad := range []SendRequest{
		{Exec: turn.Execution, ExecToken: "not-the-token"},
		{Exec: turn.Execution},
		{Exec: "made-up-execution", ExecToken: turn.Token},
	} {
		bad.To, bad.Subject, bad.Body = "nova", "Forged", "x"
		if _, err := rp.f.bus.Send("atlas", bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	if _, err := rp.f.bus.Send("nova", SendRequest{To: "atlas", Subject: "Mine", Body: "x", Exec: turn.Execution, ExecToken: turn.Token}); err == nil {
		t.Error("another agent's execution was accepted")
	}
	mustSend(t, rp.f, "atlas", SendRequest{To: "nova", Subject: "Plain", Body: "x"})
	var n int
	_ = rp.f.db.QueryRow(`SELECT COUNT(*) FROM returns`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d returns: a send without its turn opened one", n)
	}
	if rp.r.Verified("atlas", turn.Execution, "not-the-token") != "" || rp.r.Verified("atlas", turn.Execution, turn.Token) != turn.Execution {
		t.Error("Verified")
	}
}

// The runtime admitted and ran the update's turn but its answer was lost: the core asks again with
// the same execution, and the turn runs once.
func TestReturnLostAdmissionRunsOnce(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Here they are.\n[DONE]" })
	req, _ := rp.request(t)
	rp.ch.mu.Lock()
	rp.ch.lose = 1
	rp.ch.mu.Unlock()
	rp.answer(t, req)
	rp.rounds(2)
	rp.f.now = rp.f.now.Add(31 * time.Second)
	rp.rounds(3)
	if rp.runs["return"] != 1 || len(rp.poster.sent()) != 1 || rp.state(t) != ReturnDone {
		t.Fatalf("runs %v, posts %v, state %s", rp.runs, rp.poster.sent(), rp.state(t))
	}
}

// A turn the runtime no longer knows may have acted: it is given back to the bus, never rerun.
func TestReturnLostTurnIsNotRerun(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Here they are.\n[DONE]" })
	req, _ := rp.request(t)
	rp.answer(t, req)
	rp.r.applyEvents()
	rp.r.drive() // started
	rp.ch.mu.Lock()
	rp.ch.status = map[string]engine.ChannelTurnStatus{} // the runtime lost its store
	rp.ch.mu.Unlock()
	rp.rounds(3)
	if rp.runs["return"] != 1 || len(rp.poster.sent()) != 0 {
		t.Fatalf("runs %v, posts %v", rp.runs, rp.poster.sent())
	}
	if got := findingRules(t, rp.f.db); !strings.Contains(strings.Join(got, ","), "atlas:return_undelivered") {
		t.Errorf("findings %v", got)
	}
}

// A part that went but could not be journaled is recorded, never sent again.
func TestReturnSentButNotJournaled(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Here they are.\n[DONE]" })
	req, _ := rp.request(t)
	rp.poster.unrecordedAt = 0
	rp.answer(t, req)
	rp.rounds(4)
	if len(rp.poster.sent()) != 1 || rp.state(t) != ReturnDone {
		t.Fatalf("posts %v, state %s", rp.poster.sent(), rp.state(t))
	}
}

// A long update held at its second part: resolving it as sent posts only the third; as not sent,
// the second and the third. The update completes only once every part went.
func TestReturnResolveResumesTheUnsentParts(t *testing.T) {
	long := strings.Repeat("a", 3500) + strings.Repeat("b", 3500) + "ccc"
	for _, sent := range []bool{true, false} {
		rp := newReplay(t, func(engine.ChannelTurn) string { return long + "\n[DONE]" })
		req, _ := rp.request(t)
		rp.poster.uncertainAt = 1
		rp.answer(t, req)
		rp.rounds(4)
		var id int64
		_ = rp.f.db.QueryRow(`SELECT id FROM returns WHERE agent = 'atlas'`).Scan(&id)
		rows, _ := rp.r.List("atlas", 0)
		if rp.state(t) != ReturnUnknown || len(rows) != 1 || strings.Join(rows[0].Held, ",") != "sent,unknown,planned" {
			t.Fatalf("sent=%v: state %s, listed %+v", sent, rp.state(t), rows)
		}
		if err := rp.r.Resolve(id, sent); err != nil {
			t.Fatal(err)
		}
		if rp.state(t) == ReturnDone {
			t.Fatalf("sent=%v: done before its last part went", sent)
		}
		rp.rounds(4)
		posts := rp.poster.sent()
		want := []string{"a", "c"}
		if !sent {
			want = []string{"a", "b", "c"}
		}
		got := []string{}
		for _, p := range posts {
			text := p[strings.LastIndex(p, "|")+1:]
			got = append(got, text[:1])
		}
		if strings.Join(got, "") != strings.Join(want, "") || rp.state(t) != ReturnDone {
			t.Fatalf("sent=%v: posted %v, state %s", sent, got, rp.state(t))
		}
		if err := rp.r.Resolve(id, sent); err == nil {
			t.Fatalf("sent=%v: resolved twice", sent)
		}
	}
}

// An answer to a request whose return closed stays the bus's: the agent is woken for it as usual.
func TestReturnClosedReturnTakesNothing(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Done.\n[DONE]" })
	req, _ := rp.request(t)
	rp.answer(t, req)
	rp.rounds(4)
	rp.f.bus.WakeRound()
	rp.f.takeWakes()
	late := mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "One more", Body: "x", Thread: req, Answers: []int64{req}})
	var owner string
	_ = rp.f.db.QueryRow(`SELECT owner FROM deliveries WHERE message = ? AND agent = 'atlas'`, late.ID).Scan(&owner)
	if owner != "bus" {
		t.Fatalf("owner %s", owner)
	}
	rp.f.now = rp.f.now.Add(5 * time.Second)
	rp.f.bus.WakeRound()
	if w := rp.wokenAtlas(); len(w) != 1 {
		t.Fatalf("wakes %+v", w)
	}
}

// The agent opting out drains its returns: a queued update goes back to the bus. An engine that
// does not answer its negotiation is not taken for one that stopped enforcing.
func TestReturnDrainAndUnreachableEngine(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Here.\n[DONE]" })
	req, _ := rp.request(t)
	rp.ch.mu.Lock()
	rp.ch.capsErr = true
	rp.ch.mu.Unlock()
	rp.f.now = rp.f.now.Add(2 * capsTTL)
	rp.answer(t, req)
	rp.rounds(3)
	if len(rp.poster.sent()) != 1 {
		t.Fatalf("an unreachable negotiation drained it: %v", rp.poster.sent())
	}
	rp2 := newReplay(t, func(engine.ChannelTurn) string { return "Here.\n[DONE]" })
	req2, _ := rp2.request(t)
	rp2.optIn = false
	rp2.answer(t, req2)
	rp2.rounds(3)
	if len(rp2.poster.sent()) != 0 || rp2.state(t) != ReturnExpired {
		t.Fatalf("opted out: posts %v, state %s", rp2.poster.sent(), rp2.state(t))
	}
}

// A person's turn the connector stopped waiting for: its reply is posted when it ends, through the
// journal, quoting the message.
func TestReturnLateReplyIsPosted(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "[SILENT]" })
	rp.ch.mu.Lock()
	rp.ch.hold = true
	rp.ch.mu.Unlock()
	o := rp.person(t, sellersGroup, "wa-in-3", "50760000000")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := rp.r.InboundTurn(ctx, "atlas", "whatsapp-0a1b", whatsapp.Turn{Text: "and the cruise?", Origin: o}); !errors.Is(err, whatsapp.ErrPending) {
		t.Fatalf("still running: %v", err)
	}
	rp.rounds(2)
	if len(rp.poster.sent()) != 0 {
		t.Fatal("posted before the turn ended")
	}
	rp.ch.release()
	rp.rounds(3)
	posts := rp.poster.sent()
	if len(posts) != 1 || posts[0] != sellersGroup+"|wa-in-3|On it: I asked our research agent." {
		t.Fatalf("posts %v", posts)
	}
}

// While an update's turn runs for a chat, nothing posts there around it, whatever the call claims.
func TestReturnSideDoor(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "[SILENT]" })
	dm := rp.person(t, "1234567890@lid", "wa-in-9", "50769999999")
	for _, o := range []string{rp.origin, dm} {
		if _, err := rp.f.db.Exec(`INSERT INTO executions(id, agent, kind, origin, token, session, state, created) VALUES(?,?,?,?,?,?,?,0)`,
			newID(), "atlas", "return", o, newToken(), "whatsapp-x", "running"); err != nil {
			t.Fatal(err)
		}
	}
	group, _ := types.ParseJID(sellersGroup)
	for _, jid := range []types.JID{group, types.NewJID("50769999999", types.DefaultUserServer)} {
		if err := rp.r.sideDoor("atlas", jid); err == nil {
			t.Errorf("%s: posted around the update", jid)
		}
	}
	for _, jid := range []types.JID{types.NewJID("50760000000", types.DefaultUserServer), types.NewJID("50761234567", types.DefaultUserServer)} {
		if err := rp.r.sideDoor("atlas", jid); err != nil {
			t.Errorf("%s: %v", jid, err)
		}
	}
	if err := rp.r.sideDoor("nova", group); err != nil {
		t.Errorf("another agent: %v", err)
	}
}
