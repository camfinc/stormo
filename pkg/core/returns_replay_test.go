package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/whatsapp"
)

// fakeChannel is an agent's channel turns: each turn runs a script at once (inbound: a person's
// message; return: an outcome brought back), its reply the script's.
type fakeChannel struct {
	mu      sync.Mutex
	enforce bool
	script  func(t engine.ChannelTurn) (string, error)
	turns   []engine.ChannelTurn
	status  map[string]engine.ChannelTurnStatus
}

func (c *fakeChannel) Capabilities(context.Context) (engine.ChannelCaps, error) {
	return engine.ChannelCaps{Turns: true, Enforcing: c.enforce}, nil
}

func (c *fakeChannel) Start(_ context.Context, t engine.ChannelTurn) (engine.ChannelTurnStatus, error) {
	c.mu.Lock()
	if st, ok := c.status[t.Execution]; ok {
		c.mu.Unlock()
		return st, nil
	}
	c.turns = append(c.turns, t)
	c.mu.Unlock()
	text, err := c.script(t) // outside the lock: a script may send over the bus
	st := engine.ChannelTurnStatus{Execution: t.Execution, State: "ok", Text: text, Tip: t.Session}
	if err != nil {
		st = engine.ChannelTurnStatus{Execution: t.Execution, State: "failed", Error: err.Error()}
	}
	c.mu.Lock()
	c.status[t.Execution] = st
	c.mu.Unlock()
	return engine.ChannelTurnStatus{Execution: t.Execution, State: "running"}, nil
}

func (c *fakeChannel) Status(_ context.Context, exec string, _ time.Duration) (engine.ChannelTurnStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.status[exec]
	if !ok {
		return engine.ChannelTurnStatus{}, engine.ErrChannelUnknown
	}
	return st, nil
}

func (c *fakeChannel) Take(context.Context, string) error { return nil }

func (c *fakeChannel) kinds() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []string{}
	for _, t := range c.turns {
		out = append(out, t.Kind)
	}
	return out
}

// fakePoster is the agent's WhatsApp: the posts to an origin, journaled like the connector's.
type fakePoster struct {
	mu      sync.Mutex
	posts   []string
	refuse  error
	sendErr error
}

func (p *fakePoster) AllowedReply(whatsapp.Origin) error { return p.refuse }

func (p *fakePoster) PostOrigin(ctx context.Context, o whatsapp.Origin, parts []string, from int, journal func(int, string, string) error) error {
	for i := from; i < len(parts); i++ {
		if err := journal(i, "sending", ""); err != nil {
			return err
		}
		if p.sendErr != nil {
			return p.sendErr
		}
		p.mu.Lock()
		p.posts = append(p.posts, fmt.Sprintf("%s|%s|%s", o.Chat, o.MessageID, parts[i]))
		p.mu.Unlock()
		if err := journal(i, "sent", fmt.Sprintf("wa-%d", i)); err != nil {
			return err
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
}

const sellersGroup = "120363012345678901@g.us"

func newReplay(t *testing.T, returnScript func(t engine.ChannelTurn) string) *replay {
	t.Helper()
	f := newBusFixture(t, BusLimits{})
	rp := &replay{f: f, poster: &fakePoster{}}
	rp.ch = &fakeChannel{enforce: true, status: map[string]engine.ChannelTurnStatus{}}
	rp.ch.script = func(turn engine.ChannelTurn) (string, error) {
		if turn.Kind == "inbound" {
			// the agent delegates while answering the person
			_, err := f.bus.Send("atlas", SendRequest{To: "nova", Subject: "Spring promos for the sellers", Body: "List the agencies' spring promos.",
				Exec: turn.Execution, Op: turn.Execution + "/1"})
			if err != nil {
				return "", err
			}
			return "On it: I asked our research agent.", nil
		}
		return returnScript(turn), nil
	}
	rp.r = NewReturns(ReturnsOptions{DB: f.db, Bus: f.bus, Now: func() time.Time { return f.now },
		Settings: func(agent string) *manifest.Returns {
			if agent != "atlas" {
				return nil
			}
			return &manifest.Returns{UpdateAfter: 45 * time.Minute, EscalateAfter: 2 * time.Hour}
		},
		Channel: func(string) (engine.ChannelTurns, error) { return rp.ch, nil },
		Poster:  func(string) (ReturnPoster, error) { return rp.poster, nil }})
	id, err := rp.r.RegisterOrigin("atlas", whatsapp.OriginEvent{Session: "whatsapp-0a1b", Chat: sellersGroup, MessageID: "wa-in-1",
		Participant: "50760000000@s.whatsapp.net", Number: "50760000000", Name: "Seller", Group: true, Verified: true, Quote: []byte{1, 2}})
	if err != nil || id == "" {
		t.Fatalf("origin %q %v", id, err)
	}
	rp.origin = id
	ans, err := rp.r.InboundTurn(context.Background(), "atlas", "whatsapp-0a1b", whatsapp.Turn{Text: "spring promos?", Origin: id})
	if err != nil || ans.Text != "On it: I asked our research agent." {
		t.Fatalf("inbound %+v %v", ans, err)
	}
	return rp
}

func (rp *replay) request(t *testing.T) (id, thread int64) {
	t.Helper()
	if err := rp.f.db.QueryRow(`SELECT request, thread FROM returns WHERE agent = 'atlas'`).Scan(&id, &thread); err != nil {
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
	_ = rp.f.db.QueryRow(`SELECT state FROM returns WHERE agent = 'atlas'`).Scan(&s)
	return s
}

// The incident, end to end: a seller asks in a group, the agent delegates, the other agent answers;
// the answer brings the agent back into the seller's chat and its update is posted there, once,
// quoting what was asked, and the answer never also wakes it in the bus thread.
func TestReturnReplayTheIncident(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string {
		return "Three agencies have spring promos: Alpha 10%, Beta 15%, Gamma 2x1.\n[DONE]"
	})
	req, thread := rp.request(t)
	rp.f.bus.WakeRound()
	rp.f.takeWakes() // nova, for the request
	rp.f.now = rp.f.now.Add(10 * time.Minute)
	answer := mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "Spring promos", Body: "Alpha 10%, Beta 15%, Gamma 2x1.", Thread: req, Answers: []int64{req}})
	rp.f.now = rp.f.now.Add(5 * time.Second)
	rp.f.bus.WakeRound()
	for _, w := range rp.f.takeWakes() {
		if w.agent == "atlas" {
			t.Fatalf("the answer also woke the agent in the bus thread: %+v", w)
		}
	}
	rp.rounds(3)
	posts := rp.poster.sent()
	if len(posts) != 1 || posts[0] != sellersGroup+"|wa-in-1|Three agencies have spring promos: Alpha 10%, Beta 15%, Gamma 2x1." {
		t.Fatalf("posts %v", posts)
	}
	if got := rp.ch.kinds(); len(got) != 2 || got[1] != "return" {
		t.Fatalf("turns %v", got)
	}
	cont := rp.ch.turns[1].In.Text
	if !strings.Contains(cont, fmt.Sprintf("msg_read")) || !strings.Contains(cont, fmt.Sprint(answer.ID)) || strings.Contains(cont, "Gamma") {
		t.Errorf("the continuation names the answer to read, never its words: %q", cont)
	}
	if s := rp.state(t); s != ReturnDone {
		t.Fatalf("state %s", s)
	}
	rp.rounds(3)
	if len(rp.poster.sent()) != 1 {
		t.Fatal("posted again")
	}
	_ = thread
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

// The person's chat cannot be reached: the update falls back to the agent in the bus thread, once.
func TestReturnReplayFallback(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Here they are.\n[DONE]" })
	req, thread := rp.request(t)
	rp.f.bus.WakeRound()
	rp.f.takeWakes()
	rp.poster.refuse = fmt.Errorf("whatsapp: %s is no longer one of the allowed_groups", sellersGroup)
	mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "Promos", Body: "x", Thread: req, Answers: []int64{req}})
	rp.rounds(4)
	var fell []wakeCall
	for _, w := range rp.f.takeWakes() {
		if w.agent == "atlas" {
			fell = append(fell, w)
		}
	}
	if len(fell) != 1 || !strings.HasPrefix(fell[0].input, "[RETURN FALLBACK]") || fell[0].session != fmt.Sprintf("swarm-bus-%d", thread) {
		t.Fatalf("fallback wakes %+v", fell)
	}
	if len(rp.poster.sent()) != 0 {
		t.Fatal("posted to a chat it may not reach")
	}
	rp.f.now = rp.f.now.Add(time.Minute)
	rp.f.bus.WakeRound()
	for _, w := range rp.f.takeWakes() {
		if w.agent == "atlas" {
			t.Fatalf("woken twice for the fallen-back update: %+v", w)
		}
	}
	if got := findingRules(t, rp.f.db); !strings.Contains(strings.Join(got, ","), "atlas:return_undelivered") {
		t.Errorf("findings %v", got)
	}
}

// A send that may or may not have gone is held for a person: never posted again on its own.
func TestReturnReplayUnknownIsHeld(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "Here they are.\n[DONE]" })
	req, _ := rp.request(t)
	rp.poster.sendErr = context.DeadlineExceeded
	mustSend(t, rp.f, "nova", SendRequest{To: "atlas", Subject: "Promos", Body: "x", Thread: req, Answers: []int64{req}})
	rp.rounds(4)
	if s := rp.state(t); s != ReturnUnknown {
		t.Fatalf("state %s", s)
	}
	rp.poster.sendErr = nil
	rp.rounds(4)
	if len(rp.poster.sent()) != 0 {
		t.Fatal("an unknown send was retried on its own")
	}
	if got := findingRules(t, rp.f.db); !strings.Contains(strings.Join(got, ","), "atlas:return_unknown") {
		t.Errorf("findings %v", got)
	}
}

// One person's conversation per bus thread: another person's request may not be sent into it.
func TestReturnOnePersonPerThread(t *testing.T) {
	rp := newReplay(t, func(engine.ChannelTurn) string { return "[SILENT]" })
	req, _ := rp.request(t)
	other, err := rp.r.RegisterOrigin("atlas", whatsapp.OriginEvent{Session: "whatsapp-ffff", Chat: "50769999999@s.whatsapp.net", MessageID: "wa-in-2",
		Number: "50769999999", Name: "Other seller"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rp.f.db.Exec(`INSERT INTO executions(id, agent, kind, origin, session, state, created) VALUES('exec-other-1','atlas','inbound',?,?,'running',0)`, other, "whatsapp-ffff"); err != nil {
		t.Fatal(err)
	}
	if _, err := rp.f.bus.Send("atlas", SendRequest{To: "nova", Subject: "Also", Body: "y", Thread: req, Exec: "exec-other-1", Op: "exec-other-1/1"}); err == nil ||
		!strings.Contains(err.Error(), "another conversation") {
		t.Fatalf("another person's request in the thread: %v", err)
	}
}
