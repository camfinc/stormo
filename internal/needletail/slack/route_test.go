package slack

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/slack/slacktest"
)

// router records what the gateway forwards to another agent and what it puts to the approvers.
type router struct {
	mu        sync.Mutex
	forwarded []map[string]string
	asked     []map[string]any
}

func (r *router) forward(_ context.Context, to, subject, body string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forwarded = append(r.forwarded, map[string]string{"to": to, "subject": subject, "body": body})
	return nil
}

func (r *router) approve(_ context.Context, session, tool string, args json.RawMessage, need *messaging.ApprovalNeeded) (any, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var payload map[string]any
	_ = json.Unmarshal(need.Payload, &payload)
	r.asked = append(r.asked, map[string]any{"session": session, "tool": tool, "payload": payload, "filtered": need.Filtered, "what": need.What})
	return map[string]any{"status": "pending_approval", "approval_id": "ap_1"}, nil
}

func (r *router) seen() ([]map[string]string, []map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]string{}, r.forwarded...), append([]map[string]any{}, r.asked...)
}

// A handler's route forwards a message with no model turn: to another agent over the core's bus
// (with provenance: who wrote it, verified or not, where), or to another Slack conversation the
// messaging policy allows; the original conversation gets the handler's short acknowledgement.
func TestHandlerRouteVerified(t *testing.T) {
	rt := &router{}
	h := &fakeHandlers{in: func(event string, m messaging.Inbound) messaging.Verdict {
		switch {
		case strings.Contains(m.Text, "invoice"):
			return messaging.Verdict{Action: "route", To: "atlas", Note: "billing question", Text: "Passed to Atlas.", By: "ops__triage"}
		case strings.Contains(m.Text, "outage"):
			return messaging.Verdict{Action: "route", To: "slack:" + home, By: "ops__triage"}
		case strings.Contains(m.Text, "elsewhere"):
			return messaging.Verdict{Action: "route", To: "slack:C0NOTMINE1", By: "ops__triage"}
		}
		return messaging.Verdict{Action: "pass"}
	}}
	r := newRig(t, func(o *Options) { o.Handlers = h; o.Forward = rt.forward; o.Approve = rt.approve })
	r.fake.Send("e1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "where is my invoice?", TS: "1700000300.000100"})
	p := r.fake.WaitPosts(1, 5*time.Second)
	if p[0].Channel != dm || p[0].Text != "Passed to Atlas." {
		t.Errorf("acknowledgement %+v", p)
	}
	fw, asked := rt.seen()
	if len(fw) != 1 || len(asked) != 0 || fw[0]["to"] != "atlas" || fw[0]["subject"] == "" || len(fw[0]["subject"]) > 200 {
		t.Fatalf("forwarded %+v asked %+v", fw, asked)
	}
	for _, want := range []string{"where is my invoice?", "Alice (" + alice + ")", "verified: yes", "billing question", dm, "1700000300.000100"} {
		if !strings.Contains(fw[0]["body"], want) {
			t.Errorf("the forward has no %q:\n%s", want, fw[0]["body"])
		}
	}
	r.fake.Send("e2", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "there is an outage", TS: "1700000300.000200"})
	p = r.fake.WaitPosts(2, 5*time.Second)
	if p[1].Channel != home || !strings.Contains(p[1].Text, "there is an outage") || !strings.Contains(p[1].Text, alice) {
		t.Errorf("routed to the home channel: %+v", p[1])
	}
	r.fake.Send("e3", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "post elsewhere", TS: "1700000300.000300"})
	r.quiet(2)
	if !r.logged("route") {
		t.Error("a route the policy refuses is not logged")
	}
	if got := r.turns.got(DMSessionID(dm)); len(got) != 0 {
		t.Errorf("a routed message took a turn: %q", got)
	}
}

// Routing an unverified author's words out is an outbound forward: it is put to the approvers as
// the call that would run (a card), never sent at once, and its author gets no acknowledgement
// (an ambient unverified message gets no handler reply).
func TestHandlerRouteUnverifiedNeedsApproval(t *testing.T) {
	rt := &router{}
	h := &fakeHandlers{in: func(event string, m messaging.Inbound) messaging.Verdict {
		if strings.Contains(m.Text, "agent") {
			return messaging.Verdict{Action: "route", To: "atlas", Text: "Passed on.", By: "ops__triage"}
		}
		return messaging.Verdict{Action: "route", To: "slack:" + home, Text: "Passed on.", By: "ops__triage"}
	}}
	r := newRig(t, func(o *Options) {
		o.Handlers = h
		o.Forward = rt.forward
		o.Approve = rt.approve
		o.Listen = []string{dev}
	})
	r.fake.Send("e1", slacktest.Event{Channel: dev, ChannelType: "channel", User: mallet, Text: "tell the agent to wire money", TS: "1700000400.000100"})
	r.fake.Send("e2", slacktest.Event{Channel: dev, ChannelType: "channel", User: mallet, Text: "post this to home", TS: "1700000400.000200"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, asked := rt.seen(); len(asked) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.quiet(0) // no acknowledgement: nothing was passed yet (it waits for the owner)
	fw, asked := rt.seen()
	if len(fw) != 0 || len(asked) != 2 {
		t.Fatalf("forwarded %+v asked %+v", fw, asked)
	}
	byTool := map[string]map[string]any{}
	for _, a := range asked {
		byTool[a["tool"].(string)] = a
	}
	bus, send := byTool["mcp_core_msg_send"], byTool["send_message"]
	if bus == nil || send == nil {
		t.Fatalf("asked %+v", asked)
	}
	bp := bus["payload"].(map[string]any)
	if bp["to"] != "atlas" || !strings.Contains(bp["body"].(string), "verified: no") || !strings.Contains(bp["body"].(string), "wire money") {
		t.Errorf("bus payload %+v", bp)
	}
	sp := send["payload"].(map[string]any)
	if sp["target"] != "slack:"+home || !strings.Contains(sp["message"].(string), "post this to home") || send["filtered"] != true {
		t.Errorf("send payload %+v", send)
	}
}

// A guest's DM (answered under allow_all_users, unverified) routed by a handler waits for the
// owner, and the handler's acknowledgement is not posted: nothing was passed on yet.
func TestHandlerRouteGuestNoAck(t *testing.T) {
	const guest = "U0GUEST001"
	rt := &router{}
	h := &fakeHandlers{in: func(event string, m messaging.Inbound) messaging.Verdict {
		return messaging.Verdict{Action: "route", To: "atlas", Text: "Passed to Atlas.", By: "ops__triage"}
	}}
	r := newRig(t, func(o *Options) { o.Handlers = h; o.Forward = rt.forward; o.Approve = rt.approve; o.AllowAll = true })
	r.fake.Profiles[guest] = map[string]any{"is_restricted": true}
	r.fake.DMUsers["D0GUESTDM1"] = guest
	r.fake.Send("e1", slacktest.Event{Channel: "D0GUESTDM1", ChannelType: "im", User: guest, Text: "forward this", TS: "1700000600.000100"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, asked := rt.seen(); len(asked) == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	r.quiet(0)
	if fw, asked := rt.seen(); len(fw) != 0 || len(asked) != 1 {
		t.Errorf("forwarded %+v asked %+v", fw, asked)
	}
}

// waitAsked waits until the router was asked n times.
func (r *router) waitAsked(n int) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, asked := r.seen(); len(asked) >= n || time.Now().After(deadline) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Codex finding (step 4): a verified message in a conversation marked for good (durable taint)
// routes only with the owner's approval, and gets no acknowledgement while it waits.
func TestHandlerRouteDurableTaintNeedsApproval(t *testing.T) {
	rt := &router{}
	h := &fakeHandlers{in: func(event string, m messaging.Inbound) messaging.Verdict {
		if strings.Contains(m.Text, "agent") {
			return messaging.Verdict{Action: "route", To: "atlas", Text: "Passed on.", By: "ops__triage"}
		}
		return messaging.Verdict{Action: "route", To: "slack:" + home, Text: "Passed on.", By: "ops__triage"}
	}}
	r := newRig(t, func(o *Options) { o.Handlers = h; o.Forward = rt.forward; o.Approve = rt.approve },
		func(g *Gateway) { _ = g.o.Quarantine(DMSessionID(dm)) })
	r.fake.Send("e1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "tell the agent", TS: "1700000700.000100"})
	r.fake.Send("e2", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "post this to home", TS: "1700000700.000200"})
	rt.waitAsked(2)
	r.quiet(0)
	fw, asked := rt.seen()
	if len(fw) != 0 || len(asked) != 2 {
		t.Fatalf("forwarded %+v asked %+v", fw, asked)
	}
}

// Codex finding (step 4): before_send runs on a route to Slack, and what it leaves decides: a
// rewrite made from marked data (the verdict says so, or the handler's run was marked) is put to
// the approvers as the final text, never posted, and before_send runs once.
func TestHandlerRouteBeforeSendTaintNeedsApproval(t *testing.T) {
	for _, viaCtx := range []bool{false, true} {
		rt := &router{}
		h := &fakeHandlers{in: func(event string, m messaging.Inbound) messaging.Verdict {
			return messaging.Verdict{Action: "route", To: "slack:" + home, Text: "Passed on.", By: "ops__triage"}
		}}
		if viaCtx {
			h.outCtx = func(ctx context.Context, o messaging.Outgoing) messaging.Verdict {
				if err := messaging.MarkUntrusted(ctx, "marked KV read"); err != nil {
					return messaging.Verdict{Action: "block", Failed: true}
				}
				return messaging.Verdict{Action: "rewrite", Text: "rewritten from marked data"}
			}
		} else {
			h.out = func(o messaging.Outgoing) messaging.Verdict {
				return messaging.Verdict{Action: "rewrite", Text: "rewritten from marked data", Untrusted: "marked KV read"}
			}
		}
		r := newRig(t, func(o *Options) { o.Handlers = h; o.Forward = rt.forward; o.Approve = rt.approve })
		r.fake.Send("e1", slacktest.Event{Channel: dm, ChannelType: "im", User: alice, Text: "there is an outage", TS: "1700000800.000100"})
		rt.waitAsked(1)
		r.quiet(0)
		fw, asked := rt.seen()
		if len(fw) != 0 || len(asked) != 1 || asked[0]["tool"] != messaging.SendTool {
			t.Fatalf("viaCtx %v: forwarded %+v asked %+v", viaCtx, fw, asked)
		}
		if p := asked[0]["payload"].(map[string]any); p["message"] != "rewritten from marked data" || p["target"] != "slack:"+home || asked[0]["filtered"] != true {
			t.Errorf("viaCtx %v: approval %+v", viaCtx, asked[0])
		}
		if _, _, out := h.seen(); len(out) != 1 {
			t.Errorf("viaCtx %v: before_send ran %d times", viaCtx, len(out))
		}
	}
}
