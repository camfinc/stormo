package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// Kinds of follow-up posts, as the agent's before_send handlers see them (messaging.Outgoing's
// kind): a message follow-up's fixed text is a delivery, like a scheduled job's output; a prompt
// follow-up's reply in its own conversation is a reply, as the turn's would be.
const (
	FollowUpMessage = messaging.KindDelivery
	FollowUpReply   = messaging.KindReply
)

// FollowUpPost is a follow-up the agent scheduled (docs/needletail.md §4.12), due now.
type FollowUpPost struct {
	// Target in send_message's grammar; "" is Origin's own conversation.
	Target string
	// Origin is the conversation it was made in (its root session).
	Origin string
	// Kind is FollowUpMessage or FollowUpReply.
	Kind string
	Text string
}

// FollowUp posts a follow-up: the agent's before_send handlers first, then the messaging policy.
// In its own Slack conversation it is answered as the turn's reply would be: the thread counts
// as one the agent is in, and a DM is its person's conversation with the agent (the person asked
// there). Anywhere else the policy decides as for send_message. Never a broadcast.
func (g *Gateway) FollowUp(ctx context.Context, p FollowUpPost) error {
	if strings.TrimSpace(p.Text) == "" {
		return nil
	}
	if p.Target == "" {
		channel, thread, ok := Origin(p.Origin)
		if !ok {
			return fmt.Errorf("%s is not a Slack conversation", p.Origin)
		}
		target := messaging.Target{Platform: "slack", Channel: channel, Thread: thread}
		text, err := g.beforeSend(ctx, messaging.Outgoing{Kind: p.Kind, Session: p.Origin, Text: p.Text}, target)
		if err != nil {
			return err
		}
		if err := g.checkIn(channel, thread); err != nil {
			return err
		}
		_, err = g.postAs(ctx, channel, thread, text, false)
		return err
	}
	t, err := messaging.Parse(p.Target)
	if err != nil {
		return err
	}
	text, err := g.beforeSend(ctx, messaging.Outgoing{Kind: p.Kind, Session: p.Origin, Text: p.Text}, t)
	if err != nil {
		return err
	}
	if _, err := g.Policy().Check(t, false); err != nil {
		return err
	}
	channel := t.Channel
	switch {
	case t.User != "":
		if channel, err = g.o.Web.OpenDM(ctx, t.User); err != nil {
			return err
		}
	case channel == "":
		channel = g.o.HomeChannel
	}
	_, err = g.postAs(ctx, channel, t.Thread, text, false)
	return err
}

// CheckFollowUp is the messaging policy for a follow-up's target ("" its own conversation,
// origin), as FollowUp applies it: what follow_up create checks before it queues one.
func (g *Gateway) CheckFollowUp(target, origin string) error {
	if target == "" {
		channel, thread, ok := Origin(origin)
		if !ok {
			return errors.New("this is not a Slack conversation")
		}
		return g.checkIn(channel, thread)
	}
	t, err := messaging.Parse(target)
	if err != nil {
		return err
	}
	_, err = g.Policy().Check(t, false)
	return err
}

// checkIn is the messaging policy for a post in a conversation the agent is in (a follow-up's
// own): its thread counts as engaged, and a DM, its person's conversation with the agent, is
// answered there as a reply would be.
func (g *Gateway) checkIn(channel, thread string) error {
	if strings.HasPrefix(channel, "D") && channel != g.o.HomeChannel {
		return nil
	}
	policy := g.Policy()
	engaged := policy.Engaged
	policy.Engaged = func(c, t string) bool {
		return c == channel && (t == "" || t == thread) || engaged != nil && engaged(c, t)
	}
	_, err := policy.Check(messaging.Target{Platform: "slack", Channel: channel, Thread: thread}, false)
	return err
}
