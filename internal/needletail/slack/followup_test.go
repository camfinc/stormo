package slack

import (
	"context"
	"errors"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// A follow-up in its own Slack conversation is posted there as a reply would be (a thread of a
// channel the agent is in, a DM even of a person allowed_users does not name, who wrote there);
// anywhere else the messaging policy decides as for send_message. before_send sees each first,
// with the follow-up's kind.
func TestFollowUp(t *testing.T) {
	h := &fakeHandlers{out: func(o messaging.Outgoing) messaging.Verdict {
		if o.Text == "block me" {
			return messaging.Verdict{Action: "block", Reason: "no"}
		}
		return messaging.Verdict{Action: "allow"}
	}}
	r := newRig(t, func(o *Options) { o.Handlers = h })
	ctx := context.Background()
	thread := SessionID(dev, "1700000100.000100")
	for _, p := range []FollowUpPost{
		{Origin: thread, Kind: FollowUpMessage, Text: "In the thread, token sekret-value"},
		{Origin: DMSessionID("D0STRANGER"), Kind: FollowUpReply, Text: "In a DM it was asked in."},
		{Target: "slack", Origin: thread, Kind: FollowUpMessage, Text: "To the home channel."},
		{Target: "slack:" + alice, Kind: FollowUpMessage, Text: "To Alice's DM."},
		{Origin: thread, Kind: FollowUpMessage, Text: "  "},
	} {
		if err := r.g.FollowUp(ctx, p); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	posts := r.fake.Posts()
	if len(posts) != 4 || posts[0].Channel != dev || posts[0].Thread != "1700000100.000100" || posts[0].Text != "In the thread, token [REDACTED]" ||
		posts[1].Channel != "D0STRANGER" || posts[1].Thread != "" || posts[2].Channel != home || posts[3].Channel != "D00ALLOWED1" {
		t.Errorf("posts %+v", posts)
	}
	_, _, out := h.seen()
	if len(out) != 4 || out[0].Kind != messaging.KindDelivery || out[1].Kind != messaging.KindReply || out[0].Session != thread || out[0].ThreadTS != "1700000100.000100" {
		t.Errorf("before_send saw %+v", out)
	}
	for _, p := range []FollowUpPost{
		{Target: "slack:C0SECRET01", Origin: thread, Text: "not my channel"},
		{Target: "slack:" + mallet, Text: "not an allowed user"},
	} {
		if err := r.g.FollowUp(ctx, p); !errors.Is(err, messaging.ErrRefused) {
			t.Errorf("%+v: %v", p, err)
		}
	}
	if err := r.g.FollowUp(ctx, FollowUpPost{Origin: thread, Kind: FollowUpMessage, Text: "block me"}); !errors.Is(err, messaging.ErrBlocked) {
		t.Errorf("a blocked follow-up: %v", err)
	}
	if err := r.g.FollowUp(ctx, FollowUpPost{Origin: "chat-1", Text: "x"}); err == nil {
		t.Error("an API conversation through Slack")
	}
	r.quiet(4)
	// the check follow_up create runs is the same
	for _, c := range []struct {
		target, origin string
		ok             bool
	}{
		{"", thread, true}, {"", DMSessionID("D0STRANGER"), true}, {"", "chat-1", false},
		{"slack", "chat-1", true}, {"slack:" + alice, "", true}, {"slack:" + mallet, thread, false}, {"slack:C0SECRET01", thread, false},
	} {
		if err := r.g.CheckFollowUp(c.target, c.origin); (err == nil) != c.ok {
			t.Errorf("%q from %q: %v", c.target, c.origin, err)
		}
	}
}
