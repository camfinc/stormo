package slack

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sched"
)

// Item 3 (Codex 6): a tainted run's send elsewhere goes through before_send BEFORE it is put to a
// person: the approval binds the message as the handlers left it, and the approved send posts
// exactly that, without running the handlers again (they never act under the endorsement). A
// held report a person approved is posted only as approved: a handler that would change or block
// it now keeps it back.
func TestApprovedSendIsBoundAfterBeforeSend(t *testing.T) {
	var calls atomic.Int32
	h := &fakeHandlers{out: func(o messaging.Outgoing) messaging.Verdict {
		if calls.Add(1) == 1 {
			return messaging.Verdict{Action: "rewrite", Text: o.Text + " [signed]"}
		}
		return messaging.Verdict{Action: "rewrite", Text: "EVIL rewrite under the endorsement"}
	}}
	r := newRig(t, func(o *Options) { o.Handlers = h })
	const session = "slack-" + dev + "-1700000000.000100"
	ctx := messaging.WithUntrusted(messaging.WithSession(context.Background(), session), "a tainted run")
	_, err := r.g.Send(ctx, messaging.Request{Target: "slack", Text: "The summary."})
	need, ok := messaging.AsApproval(err)
	if !ok {
		t.Fatalf("a tainted send elsewhere: %v", err)
	}
	var bound map[string]any
	if err := json.Unmarshal(need.Payload, &bound); err != nil || bound["message"] != "The summary. [signed]" || bound["target"] != "slack" || !need.Filtered {
		t.Fatalf("the approval does not bind the filtered message: %s (%v) filtered %v", need.Payload, err, need.Filtered)
	}
	// the approved send: exactly the bound text, no handler run again
	ectx := messaging.WithFiltered(messaging.WithEndorsed(messaging.WithSession(context.Background(), session), "ap_1"))
	if _, err := r.g.Send(ectx, messaging.Request{Target: "slack", Text: bound["message"].(string)}); err != nil {
		t.Fatal(err)
	}
	posts := r.fake.WaitPosts(1, 5_000_000_000)
	if posts[0].Text != "The summary. [signed]" || calls.Load() != 1 {
		t.Fatalf("posted %q after %d handler runs", posts[0].Text, calls.Load())
	}
	// a held report approved: a handler that now rewrites it keeps it back (never posted altered)
	err = r.g.Deliver(messaging.WithEndorsed(context.Background(), "ap_2"), sched.Delivery{JobID: "j1", Target: sched.Target{Platform: "slack"}, Text: "The approved report.", Trusted: true})
	if err == nil {
		t.Fatal("an approved report a handler rewrote was delivered")
	}
	for _, p := range r.fake.Posts() {
		if strings.Contains(p.Text, "EVIL") || strings.Contains(p.Text, "approved report") {
			t.Fatalf("posted %q", p.Text)
		}
	}
}

// Codex finding (step 5): a send from a clean run whose before_send handler reads marked data and
// rewrites it is decided again on what the handlers left: the approval binds the final rewritten
// text (Filtered), so the card shows what would be posted and approving it posts exactly that,
// once, without running the handlers again. A clean rewrite is posted as before.
func TestSendMarkedByBeforeSendBindsFinalText(t *testing.T) {
	const session = "slack-" + dev + "-1700000000.000100"
	for _, viaCtx := range []bool{false, true} {
		var calls atomic.Int32
		h := &fakeHandlers{outCtx: func(ctx context.Context, o messaging.Outgoing) messaging.Verdict {
			if calls.Add(1) > 1 {
				return messaging.Verdict{Action: "rewrite", Text: "EVIL rewrite under the endorsement"}
			}
			if viaCtx {
				if err := messaging.MarkUntrusted(ctx, "marked KV read"); err != nil {
					return messaging.Verdict{Action: "block", Failed: true}
				}
				return messaging.Verdict{Action: "rewrite", Text: "rewritten from marked data"}
			}
			return messaging.Verdict{Action: "rewrite", Text: "rewritten from marked data", Untrusted: "marked KV read"}
		}}
		marked := map[string]bool{}
		r := newRig(t, func(o *Options) {
			o.Handlers = h
			o.Quarantine = func(id string) error { marked[id] = true; return nil }
			o.Untrusted = func(id string) bool { return marked[id] }
		})
		_, err := r.g.Send(messaging.WithSession(context.Background(), session), messaging.Request{Target: "slack", Text: "clean text"})
		need, ok := messaging.AsApproval(err)
		if !ok {
			t.Fatalf("viaCtx %v: not an approval: %v", viaCtx, err)
		}
		var bound map[string]any
		if err := json.Unmarshal(need.Payload, &bound); err != nil || bound["message"] != "rewritten from marked data" || bound["target"] != "slack" || !need.Filtered {
			t.Fatalf("viaCtx %v: the approval does not bind the final text: %s (%v) filtered %v", viaCtx, need.Payload, err, need.Filtered)
		}
		if n := len(r.fake.Posts()); n != 0 {
			t.Fatalf("viaCtx %v: posted before approval: %+v", viaCtx, r.fake.Posts())
		}
		ectx := messaging.WithFiltered(messaging.WithEndorsed(messaging.WithSession(context.Background(), session), "ap_1"))
		if _, err := r.g.Send(ectx, messaging.Request{Target: bound["target"].(string), Text: bound["message"].(string)}); err != nil {
			t.Fatalf("viaCtx %v: the approved send: %v", viaCtx, err)
		}
		posts := r.fake.WaitPosts(1, 5_000_000_000)
		if len(posts) != 1 || posts[0].Text != "rewritten from marked data" || calls.Load() != 1 {
			t.Fatalf("viaCtx %v: posted %+v after %d handler runs", viaCtx, posts, calls.Load())
		}
	}
	// clean: a rewrite that reads nothing marked is posted at once
	h := &fakeHandlers{out: func(o messaging.Outgoing) messaging.Verdict {
		return messaging.Verdict{Action: "rewrite", Text: "clean rewrite"}
	}}
	r := newRig(t, func(o *Options) { o.Handlers = h })
	if _, err := r.g.Send(messaging.WithSession(context.Background(), session), messaging.Request{Target: "slack", Text: "clean text"}); err != nil {
		t.Fatal(err)
	}
	if posts := r.fake.WaitPosts(1, 5_000_000_000); len(posts) != 1 || posts[0].Text != "clean rewrite" {
		t.Fatalf("clean %+v", posts)
	}
}
