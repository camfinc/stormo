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

// turnResult is what a scripted turn does: its reply, the tool calls it made, whether it failed.
type turnResult struct {
	text  string
	acted int64
	err   error
}

// fakeChannel is an agent's channel turns: each turn runs a script once, when it is admitted. lose
// admits and runs that many Starts but answers them with a transport error; busy answers that many
// with ErrChannelBusy; capsErr has the engine not answer its negotiation; hold keeps inbound turns
// running until released.
type fakeChannel struct {
	mu      sync.Mutex
	enforce bool
	capsErr bool
	lose    int
	busy    int
	down    bool // every Start fails in transport, nothing admitted
	holdAll bool // every turn, not only inbound ones, runs until released
	takes   int  // Takes that succeeded
	takeErr int  // Takes that fail first
	hold    bool
	script  func(t engine.ChannelTurn) turnResult
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
	if c.busy > 0 {
		c.busy--
		c.mu.Unlock()
		return engine.ChannelTurnStatus{}, engine.ErrChannelBusy
	}
	if c.down {
		c.mu.Unlock()
		return engine.ChannelTurnStatus{}, errors.New("POST /v1/channel/turns: connection refused")
	}
	c.turns = append(c.turns, t)
	c.mu.Unlock()
	res := c.script(t) // outside the lock: a script may send over the bus
	st := engine.ChannelTurnStatus{Execution: t.Execution, State: "ok", Text: res.text, Tip: t.Session, Acted: res.acted}
	if res.err != nil {
		st = engine.ChannelTurnStatus{Execution: t.Execution, State: "failed", Error: res.err.Error(), Acted: res.acted}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.hold && t.Kind == "inbound" || c.holdAll {
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

func (c *fakeChannel) Take(context.Context, string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.takeErr > 0 {
		c.takeErr--
		return errors.New("DELETE /v1/channel/turns: connection reset by peer")
	}
	c.takes++
	return nil
}

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

func (c *fakeChannel) set(f func(c *fakeChannel)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f(c)
}

// fakePoster is the agent's WhatsApp, posting like the connector: the post rechecked and the
// journal told before the message goes, and after; a stop reported as a *whatsapp.PostError.
// refuse: the chat may not be posted to; uncertain: the next send may or may not go; unrecorded:
// the next send goes but its journal entry fails; unsent: the next send provably does not go.
type fakePoster struct {
	mu                            sync.Mutex
	posts                         []string
	refuse                        error
	uncertain, unrecorded, unsent bool
}

func (p *fakePoster) AllowedReply(whatsapp.Origin) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refuse
}

func (p *fakePoster) PostOrigin(ctx context.Context, o whatsapp.Origin, text string, journal func(string, string) error) error {
	if err := p.AllowedReply(o); err != nil {
		return &whatsapp.PostError{Unsent: true, Err: err}
	}
	if err := journal("sending", ""); err != nil {
		return &whatsapp.PostError{Unsent: true, Err: err}
	}
	p.mu.Lock()
	switch {
	case p.uncertain:
		p.uncertain = false
		p.mu.Unlock()
		return &whatsapp.PostError{Err: context.DeadlineExceeded}
	case p.unsent:
		p.unsent = false
		p.mu.Unlock()
		return &whatsapp.PostError{Unsent: true, Err: errors.New("server returned error 479")}
	}
	p.posts = append(p.posts, fmt.Sprintf("%s|%s|%s", o.Chat, o.MessageID, text))
	id := fmt.Sprintf("wa-%d", len(p.posts))
	unrecorded := p.unrecorded
	p.unrecorded = false
	p.mu.Unlock()
	if unrecorded {
		return &whatsapp.PostError{Sent: true, ID: id, Err: errors.New("database is locked")}
	}
	if err := journal("sent", id); err != nil {
		return &whatsapp.PostError{Sent: true, ID: id, Err: err}
	}
	return nil
}

func (p *fakePoster) sent() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.posts...)
}

func (p *fakePoster) set(f func(p *fakePoster)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(p)
}

type replay struct {
	f      *busFixture
	r      *Returns
	ch     *fakeChannel
	poster *fakePoster
	origin string
	mu     sync.Mutex
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

// newReplay is the incident's start: a seller asks atlas in a group; atlas's turn asks nova. Later
// turns (return, recovery) run script.
func newReplay(t *testing.T, script func(t engine.ChannelTurn) turnResult) *replay {
	t.Helper()
	f := newBusFixture(t, BusLimits{})
	rp := &replay{f: f, poster: &fakePoster{}, optIn: true, runs: map[string]int{}}
	rp.ch = &fakeChannel{enforce: true, status: map[string]engine.ChannelTurnStatus{}, held: map[string]engine.ChannelTurnStatus{}}
	rp.ch.script = func(turn engine.ChannelTurn) turnResult {
		rp.mu.Lock()
		rp.runs[turn.Kind]++
		rp.mu.Unlock()
		if turn.Kind == "inbound" {
			if err := asked(f, turn); err != nil {
				return turnResult{err: err}
			}
			return turnResult{text: "On it: I asked our research agent.", acted: 1}
		}
		return script(turn)
	}
	rp.r = NewReturns(ReturnsOptions{DB: f.db, Bus: f.bus, Now: func() time.Time { return f.now },
		Settings: func(agent string) *manifest.Returns {
			rp.mu.Lock()
			defer rp.mu.Unlock()
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

func reply(text string) func(engine.ChannelTurn) turnResult {
	return func(engine.ChannelTurn) turnResult { return turnResult{text: text} }
}

func (rp *replay) ran(kind string) int {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	return rp.runs[kind]
}

func (rp *replay) setOptIn(on bool) {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	rp.optIn = on
}

// person records a person's message in chat as an origin.
func (rp *replay) person(t *testing.T, chat, msgID, number string) string {
	t.Helper()
	id, err := rp.r.RegisterOrigin("atlas", whatsapp.OriginEvent{Session: "whatsapp-" + msgID, Chat: chat, MessageID: msgID,
		Participant: number + "@s.whatsapp.net", Number: number, Name: "Seller " + number, Group: strings.HasSuffix(chat, "@g.us"),
		Verified: true, Quote: []byte{1, 2}, Account: "50760001111:3@s.whatsapp.net"})
	if err != nil || id == "" {
		t.Fatalf("origin %q %v", id, err)
	}
	return id
}

// execution is a turn of kind for origin the core started, with its token.
func (rp *replay) execution(t *testing.T, origin, kind string) engine.ChannelTurn {
	t.Helper()
	turn := engine.ChannelTurn{Execution: newID(), Token: newToken(), Kind: kind}
	if _, err := rp.f.db.Exec(`INSERT INTO executions(id, agent, kind, origin, token, session, state, created) VALUES(?,?,?,?,?,?,?,0)`,
		turn.Execution, "atlas", kind, origin, turn.Token, "whatsapp-x", "running"); err != nil {
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

// pass is n rounds, the clock moving gap before each: what waits for a retry gets its turn.
func (rp *replay) pass(n int, gap time.Duration) {
	for i := 0; i < n; i++ {
		rp.f.now = rp.f.now.Add(gap)
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

func (rp *replay) owner(t *testing.T, msg int64) string {
	t.Helper()
	var o string
	_ = rp.f.db.QueryRow(`SELECT owner FROM deliveries WHERE message = ? AND agent = 'atlas'`, msg).Scan(&o)
	return o
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

func (rp *replay) outcomes(t *testing.T) string {
	t.Helper()
	rows, err := rp.f.db.Query(`SELECT outcome FROM return_deliveries WHERE state = 'settled' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var o string
		_ = rows.Scan(&o)
		out = append(out, o)
	}
	return strings.Join(out, ",")
}

func hasFinding(t *testing.T, db *DB, rule string) bool {
	t.Helper()
	return strings.Contains(strings.Join(findingRules(t, db), ","), "atlas:"+rule)
}

// 10. The incident, end to end: a seller asks in a group, the agent delegates, the other agent
// answers; the answer brings the agent back into the seller's chat and its update is posted there,
// once, as one message quoting what was asked, and the answer never also wakes it in the bus.
func TestReturnReplayTheIncident(t *testing.T) {
	rp := newReplay(t, reply("Three agencies have spring promos: Alpha 10%, Beta 15%, Gamma 2x1.\n[DONE]"))
	req, _ := rp.request(t)
	rp.f.bus.WakeRound()
	rp.f.takeWakes() // nova, for the request
	rp.f.now = rp.f.now.Add(10 * time.Minute)
	answer := rp.answer(t, req)
	rp.f.now = rp.f.now.Add(5 * time.Second)
	rp.f.bus.WakeRound()
	if w := rp.wokenAtlas(); len(w) != 0 {
		t.Fatalf("the answer also woke the agent in the bus: %+v", w)
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
	if s := rp.state(t); s != ReturnDone || rp.owner(t, answer.ID) != "return" {
		t.Fatalf("state %s, owner %s", s, rp.owner(t, answer.ID))
	}
	rp.rounds(3)
	if len(rp.poster.sent()) != 1 {
		t.Fatal("posted again")
	}
	rows, _ := rp.r.List("atlas", 0)
	if len(rows) != 1 || rows[0].Confirmed != 1 || rows[0].Unconfirmed+rows[0].Failed != 0 {
		t.Fatalf("listed %+v", rows)
	}
}

// No answer by the deadline: the agent is brought back to tell the person where it stands.
func TestReturnReplayDeadline(t *testing.T) {
	rp := newReplay(t, reply("Still checking with the agencies; more soon.\n[UPDATE]"))
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

// 1. Capture versus closure, and [DONE] that has not caught up: an answer arriving while a progress
// update runs keeps the return open after a [DONE] and goes next; ownership never changes; an
// answer after closing stays the bus's.
func TestReturnCaptureAndClosure(t *testing.T) {
	rp := newReplay(t, reply("Here is what I have.\n[DONE]"))
	req, _ := rp.request(t)
	progress := mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "Working on it", Body: "Two agencies so far.", Thread: req})
	rp.r.drive() // the progress update's turn starts and ends
	answer := rp.answer(t, req)
	rp.rounds(2) // the progress update is kept, then posted with [DONE]: not caught up
	if s := rp.state(t); s != ReturnUpdated {
		t.Fatalf("a [DONE] that never saw the answer closed it: %s", s)
	}
	rp.rounds(3)
	if s := rp.state(t); s != ReturnDone || len(rp.poster.sent()) != 2 || rp.outcomes(t) != "confirmed,confirmed" {
		t.Fatalf("state %s, posts %v, outcomes %s", s, rp.poster.sent(), rp.outcomes(t))
	}
	for _, m := range []int64{progress.ID, answer.ID} {
		if o := rp.owner(t, m); o != "return" {
			t.Fatalf("message %d went back to the %s", m, o)
		}
	}
	rp.f.bus.WakeRound()
	rp.f.takeWakes()
	late := mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "One more", Body: "x", Thread: req, Answers: []int64{req}})
	if o := rp.owner(t, late.ID); o != "bus" {
		t.Fatalf("a closed return captured: %s", o)
	}
	rp.f.now = rp.f.now.Add(5 * time.Second)
	rp.f.bus.WakeRound()
	if w := rp.wokenAtlas(); len(w) != 1 {
		t.Fatalf("wakes %+v", w)
	}
	// once closed, what it owned is readable again (never woken)
	if ms, _ := rp.f.bus.InboxIn("atlas", false, 20, ""); !hasMessage(ms, answer.ID) {
		t.Fatal("a closed return's answer is hidden")
	}
}

func hasMessage(ms []Message, id int64) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// While a return is open, an ordinary turn neither lists nor reads what it captured; its own turn
// does, proven by its token.
func TestReturnVisibility(t *testing.T) {
	rp := newReplay(t, reply("[SILENT]"))
	req, _ := rp.request(t)
	rp.r.drive() // nothing yet
	var id int64
	_ = rp.f.db.QueryRow(`SELECT id FROM returns`).Scan(&id)
	own := rp.execution(t, rp.origin, "return")
	_, _ = rp.f.db.Exec(`UPDATE executions SET ret = ? WHERE id = ?`, id, own.Execution)
	answer := rp.answer(t, req)
	if ms, _ := rp.f.bus.InboxIn("atlas", false, 20, ""); hasMessage(ms, answer.ID) {
		t.Fatal("an ordinary turn lists it")
	}
	if ms, _ := rp.f.bus.InboxIn("atlas", false, 20, own.Execution); !hasMessage(ms, answer.ID) {
		t.Fatal("the return's own turn does not list it")
	}
}

// 2. Every send boundary: maybe sent settles unconfirmed and is never sent again; sent but not
// journaled settles confirmed, never resent; provably unsent goes again; a core stopped mid-send
// settles it unconfirmed at boot.
func TestReturnSendBoundaries(t *testing.T) {
	for _, c := range []struct {
		name    string
		set     func(p *fakePoster)
		posts   int
		outcome string
	}{
		{"uncertain", func(p *fakePoster) { p.uncertain = true }, 0, OutcomeUnconfirmed},
		{"unrecorded", func(p *fakePoster) { p.unrecorded = true }, 1, OutcomeConfirmed},
		{"unsent", func(p *fakePoster) { p.unsent = true }, 1, OutcomeConfirmed},
	} {
		rp := newReplay(t, reply("Here they are.\n[UPDATE]"))
		req, _ := rp.request(t)
		rp.poster.set(c.set)
		rp.answer(t, req)
		rp.pass(6, 61*time.Second) // each retry's wait passes
		if got := len(rp.poster.sent()); got != c.posts || rp.outcomes(t) != c.outcome {
			t.Fatalf("%s: %d posts, outcomes %s", c.name, got, rp.outcomes(t))
		}
		if c.outcome == OutcomeUnconfirmed && !hasFinding(t, rp.f.db, "return_unconfirmed") {
			t.Fatalf("%s: no finding", c.name)
		}
	}
	rp := newReplay(t, reply("Here they are.\n[UPDATE]"))
	req, _ := rp.request(t)
	rp.answer(t, req)
	rp.r.drive()                                                                                   // started and ended
	rp.r.drive()                                                                                   // prepared
	_, _ = rp.f.db.Exec(`UPDATE return_deliveries SET state = 'sending' WHERE state = 'prepared'`) // the core stops mid-send
	restarted := NewReturns(rp.r.o)
	restarted.recover()
	if rp.outcomes(t) != OutcomeUnconfirmed || len(rp.poster.sent()) != 0 {
		t.Fatalf("after a stop mid-send: %s, posts %v", rp.outcomes(t), rp.poster.sent())
	}
}

// 7. An unconfirmed [DONE] stays receptive: no reminder replays it, a later answer still reaches
// the person. A confirmed but truncated [DONE] cannot close.
func TestReturnUnconfirmedDoneAndTruncation(t *testing.T) {
	rp := newReplay(t, reply("Here they are.\n[DONE]"))
	req, _ := rp.request(t)
	rp.poster.set(func(p *fakePoster) { p.uncertain = true })
	rp.answer(t, req)
	rp.rounds(3)
	if s := rp.state(t); s != ReturnOpen || rp.outcomes(t) != OutcomeUnconfirmed {
		t.Fatalf("state %s, outcomes %s", s, rp.outcomes(t))
	}
	rp.f.now = rp.f.now.Add(90 * time.Minute) // past the update deadline, before escalation... and after it
	rp.rounds(3)
	if rp.ran("return") != 1 {
		t.Fatalf("a reminder replayed the unconfirmed final: %d turns", rp.ran("return"))
	}
	mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "Correction", Body: "Gamma ended.", Thread: req, Answers: []int64{req}})
	rp.rounds(3)
	if len(rp.poster.sent()) != 1 || rp.state(t) != ReturnDone {
		t.Fatalf("the later answer: posts %v, state %s", rp.poster.sent(), rp.state(t))
	}

	long := newReplay(t, reply(strings.Repeat("a", maxUpdate+50)+"\n[DONE]"))
	req, _ = long.request(t)
	long.answer(t, req)
	long.rounds(3)
	posts := long.poster.sent()
	if len(posts) != 1 || !strings.HasSuffix(posts[0], "…") || len([]rune(posts[0][strings.LastIndex(posts[0], "|")+1:])) != maxUpdate {
		t.Fatalf("one message, cut short: %d posts", len(posts))
	}
	if s := long.state(t); s != ReturnUpdated {
		t.Fatalf("a truncated [DONE] closed it: %s", s)
	}
}

// 3. Admission: a lost Start response is asked again with the same execution and runs once; a turn
// the runtime lost is never rerun or recovered; one never confirmed in 30 minutes settles failed.
func TestReturnAdmission(t *testing.T) {
	rp := newReplay(t, reply("Here they are.\n[DONE]"))
	req, _ := rp.request(t)
	rp.ch.set(func(c *fakeChannel) { c.lose = 1 })
	rp.answer(t, req)
	rp.rounds(1)
	rp.f.now = rp.f.now.Add(31 * time.Second)
	rp.rounds(3)
	if rp.ran("return") != 1 || len(rp.poster.sent()) != 1 || rp.state(t) != ReturnDone {
		t.Fatalf("runs %d, posts %v, state %s", rp.ran("return"), rp.poster.sent(), rp.state(t))
	}

	lost := newReplay(t, reply("Here they are.\n[DONE]"))
	req, _ = lost.request(t)
	lost.answer(t, req)
	lost.r.drive() // started
	lost.ch.set(func(c *fakeChannel) { c.status = map[string]engine.ChannelTurnStatus{} })
	lost.rounds(3)
	if lost.ran("return") != 1 || lost.ran("recovery") != 0 || len(lost.poster.sent()) != 0 || lost.outcomes(t) != OutcomeFailed ||
		!hasFinding(t, lost.f.db, "return_undelivered") {
		t.Fatalf("lost: runs %v, posts %v, outcomes %s", lost.runs, lost.poster.sent(), lost.outcomes(t))
	}

	never := newReplay(t, reply("x\n[DONE]"))
	req, _ = never.request(t)
	never.ch.set(func(c *fakeChannel) { c.down = true })
	never.answer(t, req)
	never.rounds(1)
	never.f.now = never.f.now.Add(startPatience + time.Minute)
	never.rounds(2)
	if never.outcomes(t) != OutcomeFailed || never.ran("recovery") != 0 {
		t.Fatalf("never confirmed: %s, recovery %d", never.outcomes(t), never.ran("recovery"))
	}
}

// 6. A failed update turn: one that provably did nothing runs again, up to ReturnTurnTries; one that
// may have acted gets one recovery turn, which only reads, and whose reply goes out; a recovery that
// fails settles it failed, with the owner told.
func TestReturnRecovery(t *testing.T) {
	failing := func(acted int64) func(engine.ChannelTurn) turnResult {
		return func(t engine.ChannelTurn) turnResult {
			if t.Kind == "recovery" {
				return turnResult{text: "Something went wrong on my side; here is where it stands.\n[UPDATE]"}
			}
			return turnResult{err: errors.New("model error"), acted: acted}
		}
	}
	rp := newReplay(t, failing(2))
	req, _ := rp.request(t)
	rp.answer(t, req)
	rp.pass(6, 11*time.Second)
	kinds := rp.ch.kinds()
	if got := strings.Join(kinds[1:], ","); got != "return,recovery" {
		t.Fatalf("turns %s", got)
	}
	rec := rp.ch.turns[len(rp.ch.turns)-1]
	if !strings.Contains(rec.In.Text, "2 tool call(s)") || !strings.Contains(rec.In.Text, "can only read") {
		t.Errorf("recovery input %q", rec.In.Text)
	}
	if len(rp.poster.sent()) != 1 || rp.outcomes(t) != OutcomeConfirmed {
		t.Fatalf("posts %v, outcomes %s", rp.poster.sent(), rp.outcomes(t))
	}

	zero := newReplay(t, failing(0))
	req, _ = zero.request(t)
	zero.answer(t, req)
	zero.pass(9, 11*time.Second)
	if got := strings.Join(zero.ch.kinds()[1:], ","); got != "return,return,return,recovery" {
		t.Fatalf("zero-effect: turns %s", got)
	}

	worse := newReplay(t, func(engine.ChannelTurn) turnResult { return turnResult{err: errors.New("model error"), acted: 1} })
	req, _ = worse.request(t)
	worse.answer(t, req)
	worse.pass(6, 11*time.Second)
	if got := strings.Join(worse.ch.kinds()[1:], ","); got != "return,recovery" || worse.outcomes(t) != OutcomeFailed ||
		!hasFinding(t, worse.f.db, "return_undelivered") || len(worse.poster.sent()) != 0 {
		t.Fatalf("recovery failed: turns %s, outcomes %s", got, worse.outcomes(t))
	}
}

// A recovery turn only reads: with its own valid token, a send or a write is refused by the core.
func TestReturnRecoveryOnlyReads(t *testing.T) {
	rp := newReplay(t, reply("[SILENT]"))
	rec := rp.execution(t, rp.origin, "recovery")
	if _, err := rp.f.bus.Send("atlas", SendRequest{To: "nova", Subject: "x", Body: "y", Exec: rec.Execution, ExecToken: rec.Token}); err == nil {
		t.Fatal("a recovery turn sent a message")
	}
	call := ToolCall{Agent: "atlas", Meta: CallMeta{Exec: rec.Execution, ExecToken: rec.Token}}
	if rp.r.guard("whatsapp_send", call) == nil || rp.r.guard("fs_write", call) == nil {
		t.Fatal("a recovery turn may write")
	}
	if rp.r.guard("msg_thread", call) != nil {
		t.Fatal("a recovery turn may not read")
	}
	forged := ToolCall{Agent: "atlas", Meta: CallMeta{Exec: rec.Execution, ExecToken: "nope"}}
	if rp.r.guard("whatsapp_send", forged) != nil {
		t.Fatal("an unproven claim changed what is allowed") // not its turn: the ordinary rules apply
	}
}

// 4. Origin authorization: a thread is one person's for life; one message cannot answer two
// returns; a call names its turn only with that turn's token.
func TestReturnOriginAuthorization(t *testing.T) {
	rp := newReplay(t, reply("Done.\n[DONE]"))
	req, _ := rp.request(t)
	other := rp.execution(t, rp.person(t, "50769999999@s.whatsapp.net", "wa-in-2", "50769999999"), "inbound")
	if err := asked(rp.f, other); err != nil {
		t.Fatal(err)
	}
	var second int64
	_ = rp.f.db.QueryRow(`SELECT request FROM returns ORDER BY id DESC LIMIT 1`).Scan(&second)
	if _, err := rp.f.bus.Send("nova", SendRequest{To: "atlas", Subject: "Both", Body: "z", Answers: []int64{req, second}}); err == nil ||
		!strings.Contains(err.Error(), "different people") {
		t.Fatalf("one answer for two people: %v", err)
	}
	rp.answer(t, req)
	rp.rounds(3)
	if rp.state(t) != ReturnDone {
		t.Fatalf("state %s", rp.state(t))
	}
	// the first person's request closed: it still says whose it was
	if _, err := rp.f.bus.Send("nova", SendRequest{To: "atlas", Subject: "Both again", Body: "z", Answers: []int64{req, second}}); err == nil ||
		!strings.Contains(err.Error(), "different people") {
		t.Fatalf("a closed and an open request of two people: %v", err)
	}
	if _, err := rp.f.bus.Send("atlas", SendRequest{To: "nova", Subject: "Also", Body: "y", Thread: req, Exec: other.Execution, ExecToken: other.Token}); err == nil ||
		!strings.Contains(err.Error(), "another person's conversation") {
		t.Fatalf("another person's request in a closed thread: %v", err)
	}
	turn := rp.execution(t, rp.origin, "inbound")
	for _, bad := range []SendRequest{{Exec: turn.Execution, ExecToken: "not-the-token"}, {Exec: turn.Execution}, {Exec: "made-up", ExecToken: turn.Token}} {
		bad.To, bad.Subject, bad.Body = "nova", "Forged", "x"
		if _, err := rp.f.bus.Send("atlas", bad); err == nil {
			t.Errorf("%+v was accepted", bad)
		}
	}
	if _, err := rp.f.bus.Send("nova", SendRequest{To: "atlas", Subject: "Mine", Body: "x", Exec: turn.Execution, ExecToken: turn.Token}); err == nil {
		t.Error("another agent's execution was accepted")
	}
}

// While an update's turn runs for a chat, nothing posts there around it, whatever the call claims.
func TestReturnSideDoor(t *testing.T) {
	rp := newReplay(t, reply("[SILENT]"))
	dm := rp.person(t, "1234567890@lid", "wa-in-9", "50769999999")
	rp.execution(t, rp.origin, "return")
	rp.execution(t, dm, "recovery")
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

// Draining closes: opting out after a busy start ends that turn and reopens the side door; the chat
// no longer reachable closes the return; an engine that does not answer its negotiation is not
// taken for one that stopped enforcing.
func TestReturnDrainAndRoute(t *testing.T) {
	rp := newReplay(t, reply("Here.\n[DONE]"))
	req, _ := rp.request(t)
	rp.ch.set(func(c *fakeChannel) { c.busy = 1 })
	rp.answer(t, req)
	rp.rounds(1)
	rp.setOptIn(false)
	rp.f.now = rp.f.now.Add(16 * time.Second)
	rp.rounds(2)
	var running int
	_ = rp.f.db.QueryRow(`SELECT COUNT(*) FROM executions WHERE kind = 'return' AND ended IS NULL`).Scan(&running)
	group, _ := types.ParseJID(sellersGroup)
	if running != 0 || rp.r.sideDoor("atlas", group) != nil || rp.state(t) != ReturnExpired || rp.ran("return") != 0 ||
		rp.outcomes(t) != OutcomeClosed || !hasFinding(t, rp.f.db, "return_closed") {
		t.Fatalf("running %d, state %s, runs %d, outcomes %s", running, rp.state(t), rp.ran("return"), rp.outcomes(t))
	}

	gone := newReplay(t, reply("Here.\n[DONE]"))
	req, _ = gone.request(t)
	gone.poster.set(func(p *fakePoster) {
		p.refuse = fmt.Errorf("whatsapp: %s is no longer one of the allowed_groups", sellersGroup)
	})
	gone.answer(t, req)
	gone.rounds(3)
	if gone.state(t) != ReturnExpired || len(gone.poster.sent()) != 0 || !hasFinding(t, gone.f.db, "return_closed") {
		t.Fatalf("route gone: state %s", gone.state(t))
	}
	gone.f.bus.WakeRound()
	if w := gone.wokenAtlas(); len(w) != 0 {
		t.Fatalf("a closed return's messages were woken: %+v", w)
	}

	up := newReplay(t, reply("Here.\n[DONE]"))
	req, _ = up.request(t)
	up.ch.set(func(c *fakeChannel) { c.capsErr = true })
	up.f.now = up.f.now.Add(2 * capsTTL)
	up.answer(t, req)
	up.rounds(3)
	if len(up.poster.sent()) != 1 {
		t.Fatalf("an unreachable negotiation drained it: %v", up.poster.sent())
	}
}

// 8. Event policy: an ack is no event; the bus's "past its due time" notice is not sent for a
// request a return follows; its "Not answered" is the return's failure event.
func TestReturnEventPolicy(t *testing.T) {
	rp := newReplay(t, reply("They have not answered; I am chasing it.\n[UPDATE]"))
	turn := rp.execution(t, rp.origin, "inbound")
	due, err := rp.f.bus.Send("atlas", SendRequest{To: "nova", Subject: "Dates", Body: "By noon.", DueMinutes: 30,
		Exec: turn.Execution, ExecToken: turn.Token})
	if err != nil {
		t.Fatal(err)
	}
	acked, err := rp.f.bus.Send("atlas", SendRequest{To: "nova", Subject: "Fares", Body: "When you can.", Exec: turn.Execution, ExecToken: turn.Token})
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.f.bus.Ack("nova", acked.ID); err != nil {
		t.Fatal(err)
	}
	var events int
	_ = rp.f.db.QueryRow(`SELECT COUNT(*) FROM return_events`).Scan(&events)
	if events != 0 {
		t.Fatalf("an ack made %d events", events)
	}
	rp.f.bus.WakeRound()
	rp.f.takeWakes()
	rp.f.now = rp.f.now.Add(31 * time.Minute)
	rp.f.bus.WakeRound()
	rp.f.bus.WakeRound()
	if in, _ := rp.f.bus.InboxIn("atlas", true, 20, ""); len(in) != 0 {
		t.Fatalf("a status notice for a followed request: %+v", in)
	}
	var told int
	_ = rp.f.db.QueryRow(`SELECT told FROM deliveries WHERE message = ? AND agent = 'nova'`, due.ID).Scan(&told)
	if told == 0 {
		t.Fatal("the due time never passed for the bus: the test proves nothing")
	}
}

// 9. Retention: an answer in a thread of its own is kept while its return is open.
func TestReturnPrunePinsTheAnswersThread(t *testing.T) {
	rp := newReplay(t, reply("[SILENT]"))
	req, _ := rp.request(t)
	answer := mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "Promos", Body: "x", Answers: []int64{req}})
	if answer.Thread == req {
		t.Fatal("the answer is in the request's thread")
	}
	rp.f.now = rp.f.now.Add(100 * 24 * time.Hour)
	if err := rp.f.bus.Prune(30 * 24 * time.Hour); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = rp.f.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE id IN (?, ?)`, req, answer.ID).Scan(&n)
	if n != 2 {
		t.Fatalf("%d of the request and its answer kept", n)
	}
}

// 10. A person's turn that runs out of time is abandoned: the connector tells them it could not
// complete, the worker follows the turn to its end and posts nothing, and a request it made still
// comes back.
func TestReturnInboundTimeout(t *testing.T) {
	rp := newReplay(t, reply("Here they are.\n[DONE]"))
	rp.ch.set(func(c *fakeChannel) { c.hold = true })
	o := rp.person(t, sellersGroup, "wa-in-3", "50760000000")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := rp.r.InboundTurn(ctx, "atlas", "whatsapp-0a1b", whatsapp.Turn{Text: "and the cruise?", Origin: o}); err == nil {
		t.Fatal("a turn that ran out of time answered")
	}
	var state string
	_ = rp.f.db.QueryRow(`SELECT state FROM executions WHERE origin = ? AND kind = 'inbound'`, o).Scan(&state)
	if state != "abandoned" {
		t.Fatalf("state %s", state)
	}
	rp.ch.release()
	rp.rounds(2)
	var ended, taken int
	_ = rp.f.db.QueryRow(`SELECT ended IS NOT NULL, taken FROM executions WHERE origin = ? AND kind = 'inbound'`, o).Scan(&ended, &taken)
	if ended != 1 || taken != 1 || len(rp.poster.sent()) != 0 {
		t.Fatalf("after it ended: ended %d taken %d, posts %v", ended, taken, rp.poster.sent())
	}
	var second int64
	_ = rp.f.db.QueryRow(`SELECT request FROM returns WHERE origin = ?`, o).Scan(&second)
	rp.answer(t, second)
	rp.rounds(3)
	if posts := rp.poster.sent(); len(posts) != 1 || !strings.Contains(posts[0], "|wa-in-3|") {
		t.Fatalf("the abandoned turn's request did not come back: %v", posts)
	}
}

// A person's turn running when the core stopped is abandoned at boot; one started since is the
// connector's.
func TestReturnRecoverAbandonsAStoppedCoresTurn(t *testing.T) {
	rp := newReplay(t, reply("[SILENT]"))
	old := rp.execution(t, rp.origin, "inbound") // created 0: before this core started
	fresh := newID()
	if _, err := rp.f.db.Exec(`INSERT INTO executions(id, agent, kind, origin, token, session, state, created) VALUES(?,?,?,?,?,?,?,?)`,
		fresh, "atlas", "inbound", rp.origin, newToken(), "whatsapp-x", "running", rp.f.now.Add(time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	rp.r.recover()
	var a, b string
	_ = rp.f.db.QueryRow(`SELECT state FROM executions WHERE id = ?`, old.Execution).Scan(&a)
	_ = rp.f.db.QueryRow(`SELECT state FROM executions WHERE id = ?`, fresh).Scan(&b)
	if a != "abandoned" || b != "running" {
		t.Fatalf("old %s, fresh %s", a, b)
	}
}

// A turn that delegates long after its negotiation still opens a return: the worker keeps the
// negotiation of an agent with a turn running fresh.
func TestReturnLongTurnStillOpensAReturn(t *testing.T) {
	rp := newReplay(t, reply("Done.\n[DONE]"))
	req, _ := rp.request(t)
	rp.answer(t, req)
	rp.rounds(4)
	if rp.state(t) != ReturnDone {
		t.Fatalf("state %s", rp.state(t))
	}
	turn := rp.execution(t, rp.origin, "inbound")
	rp.f.now = rp.f.now.Add(3 * capsTTL)
	rp.rounds(1)
	if err := asked(rp.f, turn); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = rp.f.db.QueryRow(`SELECT COUNT(*) FROM returns`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d returns: the late request opened none", n)
	}
}

// The control line decides; a marker anywhere else decides nothing and is never posted.
func TestReturnDisposition(t *testing.T) {
	for _, c := range []struct{ in, disp, text string }{
		{"Here they are.\n[DONE]", DispositionDone, "Here they are."},
		{"Here they are.\n[DONE]\n\n", DispositionDone, "Here they are."},
		{"Progress.\n[UPDATE]", DispositionUpdate, "Progress."},
		{"Nothing yet.\n[SILENT]", DispositionSilent, ""},
		{"[SILENT]", DispositionSilent, ""},
		{"No marker at all.", DispositionUpdate, "No marker at all."},
		{"Still investigating [DONE]", DispositionUpdate, "Still investigating"},
		{"Still [UPDATE] investigating.\n[DONE]", DispositionUpdate, "Still investigating."},
		{"Still investigating\n[UPDATE]\n[DONE]", DispositionUpdate, "Still investigating"},
		{"[DONE]", DispositionSilent, ""},
	} {
		if d, text := disposition(c.in); d != c.disp || text != c.text {
			t.Errorf("%q: %s %q, want %s %q", c.in, d, text, c.disp, c.text)
		}
	}
}

// Closing a return while its update's turn is still running retires the turn without ending it:
// the side door stays shut for that chat until the runtime says it ended, then its result is taken.
// Nothing is posted.
func TestReturnCloseWhileTheTurnRuns(t *testing.T) {
	rp := newReplay(t, reply("Here.\n[DONE]"))
	req, _ := rp.request(t)
	rp.ch.set(func(c *fakeChannel) { c.holdAll = true })
	rp.answer(t, req)
	rp.rounds(1) // its turn starts and keeps running
	rp.setOptIn(false)
	rp.rounds(2) // the return closes
	group, _ := types.ParseJID(sellersGroup)
	if rp.state(t) != ReturnExpired {
		t.Fatalf("state %s", rp.state(t))
	}
	if rp.r.sideDoor("atlas", group) == nil {
		t.Fatal("the side door opened while the retired turn still runs")
	}
	rp.ch.set(func(c *fakeChannel) { c.takeErr = 1 }) // its first cleanup fails
	rp.ch.release()
	rp.rounds(1)
	if rp.r.sideDoor("atlas", group) != nil {
		t.Fatal("the side door stayed shut after the turn ended")
	}
	var taken int
	_ = rp.f.db.QueryRow(`SELECT taken FROM executions WHERE kind = 'return'`).Scan(&taken)
	if taken != 0 {
		t.Fatal("taken although the runtime refused")
	}
	rp.rounds(1)
	_ = rp.f.db.QueryRow(`SELECT taken FROM executions WHERE kind = 'return'`).Scan(&taken)
	if taken != 1 || len(rp.poster.sent()) != 0 {
		t.Fatalf("taken %d, posts %v", taken, rp.poster.sent())
	}
}

// A reply the runtime forgot (taken, or pruned while the core was away) is lost, never silence.
func TestReturnForgottenReplyIsNotSilence(t *testing.T) {
	rp := newReplay(t, reply("Here.\n[DONE]"))
	req, _ := rp.request(t)
	rp.answer(t, req)
	rp.r.drive() // started
	rp.ch.set(func(c *fakeChannel) {
		for id, st := range c.status {
			if st.State == "ok" && strings.Contains(st.Text, "Here.") {
				c.status[id] = engine.ChannelTurnStatus{Execution: id, State: "ok", Taken: true}
			}
		}
	})
	rp.rounds(2)
	if rp.outcomes(t) != OutcomeFailed || !hasFinding(t, rp.f.db, "return_undelivered") || len(rp.poster.sent()) != 0 {
		t.Fatalf("outcomes %s, posts %v", rp.outcomes(t), rp.poster.sent())
	}
}

// 5. An old update that fails does not take newer news down with it: its range fails, the answer
// that came meanwhile still reaches the person, and nothing is woken in the bus.
func TestReturnOldFailureNewerAnswer(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	rp := newReplay(t, func(engine.ChannelTurn) turnResult {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls <= 2 { // the progress update's turn and its recovery
			return turnResult{err: errors.New("model error"), acted: 1}
		}
		return turnResult{text: "They have it: Alpha 10%.\n[DONE]"}
	})
	req, _ := rp.request(t)
	progress := mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "Working on it", Body: "Two agencies so far.", Thread: req})
	rp.r.drive() // the progress update's turn starts and fails
	answer := rp.answer(t, req)
	rp.pass(8, 11*time.Second)
	if rp.outcomes(t) != OutcomeFailed+","+OutcomeConfirmed || len(rp.poster.sent()) != 1 || rp.state(t) != ReturnDone {
		t.Fatalf("outcomes %s, posts %v, state %s", rp.outcomes(t), rp.poster.sent(), rp.state(t))
	}
	for _, m := range []int64{progress.ID, answer.ID} {
		if o := rp.owner(t, m); o != "return" {
			t.Fatalf("message %d went to the %s", m, o)
		}
	}
	rp.f.bus.WakeRound()
	if w := rp.wokenAtlas(); len(w) != 0 {
		t.Fatalf("woken in the bus: %+v", w)
	}
}

// A send whose settlement could not be written is found sending outside a post: settled (as
// unconfirmed when the core did not see it go), never sent again.
func TestReturnStrandedSendSettles(t *testing.T) {
	rp := newReplay(t, reply("Here they are.\n[UPDATE]"))
	req, _ := rp.request(t)
	rp.answer(t, req)
	rp.r.drive()
	rp.r.drive() // prepared
	_, _ = rp.f.db.Exec(`UPDATE return_deliveries SET state = 'sending' WHERE state = 'prepared'`)
	rp.rounds(1)
	if rp.outcomes(t) != OutcomeUnconfirmed || len(rp.poster.sent()) != 0 || !hasFinding(t, rp.f.db, "return_unconfirmed") {
		t.Fatalf("outcomes %s, posts %v", rp.outcomes(t), rp.poster.sent())
	}
}

// A retired update turn (given up while it may still run) opens no path to the person: a request
// it sends with its own valid token goes as an ordinary message, with a note.
func TestReturnRetiredTurnOpensNothing(t *testing.T) {
	rp := newReplay(t, reply("x\n[DONE]"))
	req, _ := rp.request(t)
	rp.ch.set(func(c *fakeChannel) { c.down = true })
	rp.answer(t, req)
	rp.rounds(1)
	rp.f.now = rp.f.now.Add(startPatience + time.Minute)
	rp.rounds(1) // never confirmed: failed, its turn retired
	var exec, token, state string
	if err := rp.f.db.QueryRow(`SELECT id, token, state FROM executions WHERE kind = 'return'`).Scan(&exec, &token, &state); err != nil || state != "retired" {
		t.Fatalf("execution %s %v", state, err)
	}
	res, err := rp.f.bus.Send("atlas", SendRequest{To: "nova", Subject: "More", Body: "y", Exec: exec, ExecToken: token})
	if err != nil || res.Note == "" {
		t.Fatalf("send %+v %v", res, err)
	}
	var n int
	_ = rp.f.db.QueryRow(`SELECT COUNT(*) FROM returns`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d returns: the retired turn opened one", n)
	}
}

// A failed turn whose result was taken before the core recorded what it decides still decides the
// same (its tombstone keeps its state and what it did): one recovery turn runs.
func TestReturnFailedTombstoneStillRecovers(t *testing.T) {
	rp := newReplay(t, func(t engine.ChannelTurn) turnResult {
		if t.Kind == "recovery" {
			return turnResult{text: "Here is where it stands.\n[UPDATE]"}
		}
		return turnResult{err: errors.New("model error"), acted: 1}
	})
	req, _ := rp.request(t)
	rp.answer(t, req)
	rp.r.drive() // started; it failed
	rp.ch.set(func(c *fakeChannel) {
		for id, st := range c.status {
			if st.State == "failed" {
				c.status[id] = engine.ChannelTurnStatus{Execution: id, State: "failed", Acted: st.Acted, Taken: true}
			}
		}
	})
	rp.pass(4, 11*time.Second)
	if got := strings.Join(rp.ch.kinds()[1:], ","); got != "return,recovery" || len(rp.poster.sent()) != 1 {
		t.Fatalf("turns %s, posts %v", got, rp.poster.sent())
	}
}

// 2. Real settlement failures (the database refusing the write): a send that went is recorded once
// the database takes it, never sent again; one that may have gone settles unconfirmed; a core
// restarted meanwhile settles what it did not see go as unconfirmed.
func TestReturnSettlementFailures(t *testing.T) {
	refuse := `CREATE TRIGGER refuse_settle BEFORE UPDATE OF state ON return_deliveries WHEN NEW.state = 'settled' BEGIN SELECT RAISE(ABORT, 'injected'); END`
	for _, c := range []struct {
		name    string
		set     func(p *fakePoster)
		restart bool
		posts   int
		outcome string
	}{
		{"went", func(*fakePoster) {}, false, 1, OutcomeConfirmed},
		{"maybe", func(p *fakePoster) { p.uncertain = true }, false, 0, OutcomeUnconfirmed},
		{"went, then a restart", func(*fakePoster) {}, true, 1, OutcomeUnconfirmed},
	} {
		rp := newReplay(t, reply("Here they are.\n[UPDATE]"))
		req, _ := rp.request(t)
		rp.poster.set(c.set)
		rp.answer(t, req)
		rp.r.drive()
		rp.r.drive() // prepared
		if _, err := rp.f.db.Exec(refuse); err != nil {
			t.Fatal(err)
		}
		rp.rounds(3) // the post, and settlements the database refuses
		var state string
		_ = rp.f.db.QueryRow(`SELECT state FROM return_deliveries`).Scan(&state)
		if state != "sending" {
			t.Fatalf("%s: state %s", c.name, state)
		}
		_, _ = rp.f.db.Exec(`DROP TRIGGER refuse_settle`)
		w := rp.r
		if c.restart {
			w = NewReturns(rp.r.o)
			w.recover()
		}
		w.Round()
		w.Round()
		if got := len(rp.poster.sent()); got != c.posts || rp.outcomes(t) != c.outcome {
			t.Fatalf("%s: %d posts, outcomes %s", c.name, got, rp.outcomes(t))
		}
	}
}
