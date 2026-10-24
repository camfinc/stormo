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
	// Thread is the DM thread it was asked in, where it is posted back ("" for the DM itself).
	Thread string
	// Users are who asked (followup.Conversation.Users), who must still be admitted.
	Users []string
}

// FollowUp posts a follow-up: the agent's before_send handlers first, then the messaging policy.
// In its own Slack conversation it is answered as the turn's reply would be: the thread counts
// as one the agent is in, and a DM is its person's conversation with the agent (the person asked
// there). Anywhere else the policy decides as for send_message. Never a broadcast.
func (g *Gateway) FollowUp(ctx context.Context, p FollowUpPost) error {
	if strings.TrimSpace(p.Text) == "" {
		return nil
	}
	if err := g.admits(p.Origin, p.Users); err != nil {
		return err
	}
	if p.Target == "" {
		channel, thread, ok := Origin(p.Origin)
		if !ok {
			return fmt.Errorf("%s is not a Slack conversation", p.Origin)
		}
		if thread == "" {
			thread = p.Thread // a DM: the thread it was asked in
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
// origin) and the admission of who asked (users), as FollowUp applies them: what follow_up create
// checks before it queues one, and serve before a prompt follow-up's turn.
func (g *Gateway) CheckFollowUp(target, origin string, users []string) error {
	if err := g.admits(origin, users); err != nil {
		return err
	}
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

// admits is whether a follow-up may still act for whom it was made: everyone who asked (users)
// still admitted (allowed_users, allow_all_users), and in a DM its person too (the gateway's, or
// Slack's conversations.info: a lookup that fails is an error to try again, not a refusal). Access
// revoked since is a refusal (messaging.ErrRefused).
func (g *Gateway) admits(origin string, users []string) error {
	for _, u := range users {
		if !g.admitted(u, u) {
			return fmt.Errorf("%w: %s, who asked for it, may no longer talk to the agent (allowed_users)", messaging.ErrRefused, u)
		}
	}
	channel, thread, ok := Origin(origin)
	if !ok || thread != "" || !strings.HasPrefix(channel, "D") || channel == g.o.HomeChannel {
		return nil
	}
	person, err := g.dmUser(channel)
	switch {
	case err != nil:
		return fmt.Errorf("whose DM %s is could not be told: %v", channel, err)
	case !g.admitted(person, ""):
		return fmt.Errorf("%w: %s is the DM of %s, who may no longer talk to the agent (allowed_users)", messaging.ErrRefused, channel, person)
	}
	return nil
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
