package slack

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
)

// A handler that panics on an ambient message gives its handler slot back: with one slot, the
// next ambient message's handlers still run.
func TestPanickingAmbientHandlerReleasesItsSlot(t *testing.T) {
	h := listenHandlers()
	in := h.in
	h.in = func(event string, m messaging.Inbound) messaging.Verdict {
		if strings.Contains(m.Text, "explode") {
			panic("the handler blew up")
		}
		return in(event, m)
	}
	r := newRig(t, func(o *Options) { o.Handlers = h; o.Listen = []string{dev}; o.AmbientHandlers = 1 })
	r.fake.Send("e1", slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "explode", TS: "1700002000.000100"})
	time.Sleep(100 * time.Millisecond)
	r.fake.Send("e2", slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "the booking", TS: "1700002000.000200"})
	if p := r.fake.WaitPosts(1, 3*time.Second); len(p) != 1 {
		t.Fatalf("the next ambient message was never handled after a handler panicked: %+v (logs %q)", p, r.logs)
	}
}

// The bot-loop guard holds with no handlers too: a bot's unmentioned replies in a listened thread
// the agent is in reach a turn at most BotTurns times per hour.
func TestBotLoopGuardWithoutHandlers(t *testing.T) {
	const peerUser, thread = "U0PEERBOT1", "1700002100.000100"
	r := newRig(t, func(o *Options) {
		o.Listen, o.AllowBots, o.BotTurns = []string{dev}, "all", 2
		o.AllowedUsers = append(o.AllowedUsers, peerUser)
	})
	r.fake.Send("m0", slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "<@" + slacktest.BotUser + "> start", TS: thread})
	r.fake.WaitPosts(1, 5*time.Second)
	for i := 1; i <= 5; i++ {
		r.fake.Send(fmt.Sprintf("b%d", i), slacktest.Event{Channel: dev, ChannelType: "channel", User: peerUser, BotID: peer,
			Text: fmt.Sprintf("bot says %d", i), TS: fmt.Sprintf("1700002100.00020%d", i), ThreadTS: thread})
		time.Sleep(60 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	n := 0
	for _, in := range r.turns.got(SessionID(dev, thread)) {
		n += strings.Count(in, "bot says")
	}
	if n != 2 {
		t.Errorf("%d bot messages reached a turn without handlers, want 2: %q", n, r.turns.got(SessionID(dev, thread)))
	}
}

// A conversation's ambient share counts the ambient messages its running batch carries, not only
// those waiting: with MaxPending 4 (a share of 2), one held in its handler leaves room for one
// more, and the rest are dropped.
func TestAmbientShareCountsInflight(t *testing.T) {
	h := newHoldHandlers()
	r := newRig(t, func(o *Options) { o.Handlers = h; o.Listen = []string{dev}; o.MaxPending = 4 })
	const thread = "1700002200.000100"
	r.fake.Send("a0", slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "hold this", TS: thread})
	for i := 0; i < 100; i++ {
		if running, _ := h.peak(); running == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	for i := 1; i <= 3; i++ {
		r.fake.Send(fmt.Sprintf("a%d", i), slacktest.Event{Channel: dev, ChannelType: "channel", User: alice, Text: "chatter", TS: fmt.Sprintf("1700002200.00020%d", i), ThreadTS: thread})
	}
	time.Sleep(100 * time.Millisecond)
	close(h.release)
	time.Sleep(200 * time.Millisecond)
	if _, seen, _ := h.seen(); len(seen) != 2 {
		t.Errorf("%d ambient messages handled in the conversation, want 2 (one running, one more within the share)", len(seen))
	}
}
