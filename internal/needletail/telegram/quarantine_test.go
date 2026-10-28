package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/slack"
)

// fakeHandlers stands in for the agent's before_send handlers: out answers each outgoing message
// given the handler's context (it may mark the run, as reading marked data does).
type fakeHandlers struct {
	mu  sync.Mutex
	out func(ctx context.Context, o messaging.Outgoing) messaging.Verdict
	ran int
}

func (f *fakeHandlers) Inbound(context.Context, string, messaging.Inbound) messaging.Verdict {
	return messaging.Verdict{Action: "pass"}
}

func (f *fakeHandlers) BeforeSend(ctx context.Context, o messaging.Outgoing) messaging.Verdict {
	f.mu.Lock()
	f.ran++
	out := f.out
	f.mu.Unlock()
	if out == nil {
		return messaging.Verdict{Action: "allow"}
	}
	return out(ctx, o)
}

func (f *fakeHandlers) runs() int { f.mu.Lock(); defer f.mu.Unlock(); return f.ran }

// markedRewrite is a before_send handler that reads a marked record and rewrites the message from
// it: the verdict says so (viaCtx false) or the handler's run is marked (viaCtx true).
func markedRewrite(viaCtx bool, text string) *fakeHandlers {
	return &fakeHandlers{out: func(ctx context.Context, o messaging.Outgoing) messaging.Verdict {
		if viaCtx {
			if err := messaging.MarkUntrusted(ctx, "marked KV read"); err != nil {
				return messaging.Verdict{Action: "block", Failed: true}
			}
			return messaging.Verdict{Action: "rewrite", Text: text}
		}
		return messaging.Verdict{Action: "rewrite", Text: text, Untrusted: "marked KV read"}
	}}
}

// Codex finding: a send from a clean run whose before_send handler reads marked data and rewrites
// it is put to a person as the final text (bound, filtered), never posted to another chat; back to
// its own conversation it is a reply and is posted; a clean rewrite is posted as rewritten.
func TestSendBeforeSendTaintNeedsApproval(t *testing.T) {
	for _, viaCtx := range []bool{false, true} {
		h := markedRewrite(viaCtx, "rewritten from marked data")
		fake, _, g := rig(t, func(o *Options) { o.Handlers = h })
		ctx := messaging.WithSession(context.Background(), "telegram--100555")
		_, err := g.Send(ctx, messaging.Request{Target: "telegram", Text: "clean text"})
		need, ok := messaging.AsApproval(err)
		if !ok {
			t.Fatalf("viaCtx %v: not an approval: %v (sent %+v)", viaCtx, err, fake.Sent())
		}
		var p map[string]any
		if err := json.Unmarshal(need.Payload, &p); err != nil || p["message"] != "rewritten from marked data" || p["target"] != "telegram" || !need.Filtered {
			t.Errorf("viaCtx %v: approval %+v payload %s", viaCtx, need, need.Payload)
		}
		if len(fake.Sent()) != 0 || h.runs() != 1 {
			t.Fatalf("viaCtx %v: marked text posted %+v (before_send ran %d times)", viaCtx, fake.Sent(), h.runs())
		}
	}
	// back to its own conversation: a reply, posted
	h := markedRewrite(false, "rewritten from marked data")
	fake, tr, g := rig(t, func(o *Options) { o.Handlers = h })
	tr.mu.Lock()
	tr.sessions["telegram--100555"] = true
	tr.mu.Unlock()
	ctx := messaging.WithSession(context.Background(), "telegram--100555")
	if _, err := g.Send(ctx, messaging.Request{Target: "telegram:-100555", Text: "clean text"}); err != nil {
		t.Fatalf("a marked rewrite back to its own chat: %v", err)
	}
	if sent := fake.Sent(); len(sent) != 1 || sent[0].Chat != -100555 || sent[0].Text != "rewritten from marked data" {
		t.Fatalf("own chat %+v", sent)
	}
	// a clean rewrite elsewhere: posted as rewritten
	h = &fakeHandlers{out: func(context.Context, messaging.Outgoing) messaging.Verdict {
		return messaging.Verdict{Action: "rewrite", Text: "clean rewrite"}
	}}
	fake, _, g = rig(t, func(o *Options) { o.Handlers = h })
	if _, err := g.Send(messaging.WithSession(context.Background(), "telegram--100555"), messaging.Request{Target: "telegram", Text: "clean text"}); err != nil {
		t.Fatal(err)
	}
	if sent := fake.Sent(); len(sent) != 1 || sent[0].Chat != alice || sent[0].Text != "clean rewrite" {
		t.Fatalf("clean %+v", sent)
	}
}

// Codex finding: a scheduled report (or failure notice) whose before_send handler reads marked
// data and rewrites it is held (Deferred, for approval), never posted; a clean one is posted.
func TestDeliverBeforeSendTaintHeld(t *testing.T) {
	for _, viaCtx := range []bool{false, true} {
		for _, failure := range []bool{false, true} {
			h := markedRewrite(viaCtx, "report from marked data")
			fake, _, g := rig(t, func(o *Options) { o.Handlers = h })
			err := g.Deliver(context.Background(), sched.Delivery{Session: "cron-daily", Target: sched.Target{Platform: Source}, Text: "clean report", Trusted: true, Failure: failure})
			if !errors.Is(err, messaging.ErrRefused) {
				t.Errorf("viaCtx %v failure %v: not held: %v", viaCtx, failure, err)
			}
			if sent := fake.Sent(); len(sent) != 0 {
				t.Fatalf("viaCtx %v failure %v: marked report posted %+v", viaCtx, failure, sent)
			}
		}
	}
	h := &fakeHandlers{out: func(context.Context, messaging.Outgoing) messaging.Verdict {
		return messaging.Verdict{Action: "rewrite", Text: "clean rewritten report"}
	}}
	fake, _, g := rig(t, func(o *Options) { o.Handlers = h })
	if err := g.Deliver(context.Background(), sched.Delivery{Session: "cron-daily", Target: sched.Target{Platform: Source}, Text: "clean report", Trusted: true}); err != nil {
		t.Fatal(err)
	}
	if sent := fake.Sent(); len(sent) != 1 || sent[0].Chat != alice || sent[0].Text != "clean rewritten report" {
		t.Fatalf("clean %+v", sent)
	}
}

// A follow-up to another chat whose before_send handler reads marked data marks its destination
// before it is posted (the follow-up path's rule; it already re-checks after the handlers).
func TestFollowUpBeforeSendTaintMarksDestination(t *testing.T) {
	h := markedRewrite(true, "a reminder from marked data")
	fake, tr, g := rig(t, func(o *Options) { o.Handlers = h })
	tr.mu.Lock()
	tr.sessions["telegram--100555"] = true
	tr.mu.Unlock()
	if err := g.FollowUp(context.Background(), slack.FollowUpPost{Target: "telegram:-100555", Origin: "telegram-1001", Kind: slack.FollowUpMessage, Text: "clean"}); err != nil {
		t.Fatal(err)
	}
	if !g.o.Untrusted("telegram--100555") {
		t.Error("the destination of a follow-up rewritten from marked data is not marked")
	}
	if sent := fake.Sent(); len(sent) != 1 || sent[0].Text != "a reminder from marked data" {
		t.Fatalf("sent %+v", sent)
	}
}
