package slack

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sched"
)

func TestOwnMarkedAssistantContextQuarantinesReader(t *testing.T) {
	lines := []messaging.ContextLine{{User: "UBOT", Name: "assistant", Text: "marked assistant text", Assistant: true, Unverified: true}}
	in := inputOf(nil, lines)
	if !in.ContextUnverified {
		t.Fatal("marked own-bot context lost provenance")
	}
	if text := threadContext(lines); !strings.Contains(text, "[unverified]") {
		t.Fatalf("marked assistant framing lost: %s", text)
	}
}

func TestMarkedFollowUpPremarkAndFailure(t *testing.T) {
	var r *rig
	var marked []string
	fail := false
	r = newRig(t, func(o *Options) {
		o.Quarantine = func(session string) error {
			if len(r.fake.Posts()) != len(marked) {
				t.Fatal("post happened before destination mark")
			}
			if fail {
				return errors.New("forced append failure")
			}
			marked = append(marked, session)
			return nil
		}
	})
	for _, p := range []FollowUpPost{
		{Target: "slack:" + alice, Text: "private marked reminder", Untrusted: true},
		{Target: "slack:" + dev + ":1700000100.000100", Text: "thread marked reminder", Untrusted: true},
	} {
		if strings.Contains(p.Target, dev) {
			r.g.threads.add(dev + ":1700000100.000100")
		}
		if err := r.g.FollowUp(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	if len(marked) != 2 || marked[0] != DMSessionID("D00ALLOWED1") || marked[1] != SessionID(dev, "1700000100.000100") {
		t.Fatalf("destinations=%v", marked)
	}
	fail = true
	if err := r.g.FollowUp(context.Background(), FollowUpPost{Target: "slack:" + alice, Text: "not posted", Untrusted: true}); err == nil {
		t.Fatal("mark failure ignored")
	}
	if len(r.fake.Posts()) != 2 {
		t.Fatal("mark failure posted text")
	}
	if err := r.g.FollowUp(context.Background(), FollowUpPost{Target: "slack", Text: "no known thread", Untrusted: true}); err == nil {
		t.Fatal("marked top-level channel post allowed")
	}
	if len(r.fake.Posts()) != 2 {
		t.Fatal("top-level marked post escaped")
	}
}

func TestOwnBotThreadContextReadsPersistentMark(t *testing.T) {
	r := newRig(t, func(o *Options) {
		o.Untrusted = func(session string) bool { return session == SessionID(dev, "1700000100.000100") }
	})
	r.fake.Replies[dev+":1700000100.000100"] = []map[string]any{{"user": r.g.id.UserID, "text": "previous marked reply", "ts": "1700000100.000200"}}
	lines := r.g.threadLines(dev, "1700000100.000100", nil)
	if len(lines) != 1 || !lines[0].Assistant || !lines[0].Unverified {
		t.Fatalf("context=%+v", lines)
	}
	if !inputOf(nil, lines).ContextUnverified {
		t.Fatal("own context provenance lost")
	}
}

func TestQuarantinedSendOwnOriginOnly(t *testing.T) {
	r := newRig(t, nil)
	source := SessionID(dev, "1700000100.000100")
	r.g.threads.add(dev + ":1700000100.000100")
	ctx := messaging.WithSession(messaging.WithUntrusted(context.Background(), "marked source"), source)
	if _, err := r.g.Send(ctx, messaging.Request{Target: "slack:" + dev + ":1700000100.000100", Text: "own reply"}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"slack", "slack:" + dev, "slack:" + alice, "slack:" + dev + ":1700000100.000300"} {
		if _, err := r.g.Send(ctx, messaging.Request{Target: target, Text: "forwarded"}); err == nil {
			t.Errorf("quarantined forwarding allowed to %s", target)
		}
	}
	if len(r.fake.Posts()) != 1 {
		t.Fatal("forwarded marked text")
	}
}

func TestBeforeSendMarkedResultCannotEscape(t *testing.T) {
	marked := map[string]bool{}
	h := &fakeHandlers{out: func(messaging.Outgoing) messaging.Verdict {
		return messaging.Verdict{Action: "rewrite", Text: "marked rewritten text", Untrusted: "marked KV read"}
	}}
	r := newRig(t, func(o *Options) {
		o.Handlers = h
		o.Quarantine = func(id string) error { marked[id] = true; return nil }
		o.Untrusted = func(id string) bool { return marked[id] }
	})
	source := SessionID(dev, "1700000100.000100")
	ctx := messaging.WithSession(context.Background(), source)
	if _, err := r.g.Send(ctx, messaging.Request{Target: "slack", Text: "initial clean text"}); err == nil {
		t.Fatal("before_send marked text forwarded")
	}
	for _, failure := range []bool{false, true} {
		if err := r.g.Deliver(context.Background(), sched.Delivery{Session: "cron-example", Target: sched.Target{Platform: "slack", Channel: home}, Text: "clean initial output", Trusted: true, Failure: failure}); err == nil {
			t.Fatal("scheduled marked output posted")
		}
	}
	if len(r.fake.Posts()) != 0 {
		t.Fatalf("marked text escaped: %+v", r.fake.Posts())
	}
}

func TestHandlerInitialQuarantineFailureStopsExecution(t *testing.T) {
	calls := 0
	h := &fakeHandlers{in: func(string, messaging.Inbound) messaging.Verdict {
		calls++
		return messaging.Verdict{Action: "reply", Text: "must not post"}
	}}
	r := newRig(t, func(o *Options) {
		o.Handlers = h
		o.Quarantine = func(string) error { return errors.New("forced initial mark failure") }
	})
	// an unverified author: the conversation is marked before the handler runs; a failed mark
	// runs nothing
	jobs := []job{{channel: dev, verified: false, ev: Message{User: mallet, Text: "unverified question"}}}
	left := r.g.dispatch(SessionID(dev, "1700000100.000100"), jobs, nil)
	if calls != 0 || len(left) != 0 || len(r.fake.Posts()) != 0 {
		t.Fatalf("initial mark failure executed handler: calls=%d left=%d", calls, len(left))
	}
	// unverified thread context alone: the handler runs, but a reply that carries the context
	// into the conversation needs the mark first; a failed mark posts nothing
	jobs = []job{{channel: dev, verified: true, ev: Message{User: alice, Text: "verified question"}}}
	left = r.g.dispatch(SessionID(dev, "1700000100.000100"), jobs, []messaging.ContextLine{{Text: "unverified context", Unverified: true}})
	if calls != 1 || len(left) != 0 || len(r.fake.Posts()) != 0 {
		t.Fatalf("a context-carrying reply escaped a failed mark: calls=%d left=%d posts=%v", calls, len(left), r.fake.Posts())
	}
}
