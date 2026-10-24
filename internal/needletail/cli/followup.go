package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/camfinc/stormo/internal/needletail/api"
	"github.com/camfinc/stormo/internal/needletail/followup"
	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/slack"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// followUpMinDelay is the shortest follow-up the agent may schedule (tests lower it).
var followUpMinDelay = followup.MinDelay

// followUpOrigin is the conversation a session belongs to, from the runtime's own records (as
// the memory's audiences): its root and source (a Slack session its handlers see before its turn
// creates it is read from its id), and its turns' memory audience. Never the model's words.
func followUpOrigin(store *sessions.Store, where func(session string) memory.Audience) func(session string) followup.Conversation {
	return func(session string) followup.Conversation {
		if session == "" {
			return followup.Conversation{}
		}
		root, source := session, ""
		if r, ok := store.Root(session); ok {
			h, _ := store.Get(r)
			root, source = r, h.Source
		} else if strings.HasPrefix(session, "slack-") {
			source = slack.Source
		}
		c := followup.Conversation{Session: root, Source: source}
		if where != nil {
			a := where(session)
			c.Operator = a.Operator
			switch {
			case a.Operator:
				c.Audience = "operator"
			case a.Scope != "":
				c.Audience = a.Scope
			}
		}
		return c
	}
}

// followUpPoster is the Slack gateway as the follow-ups use it.
type followUpPoster interface {
	FollowUp(ctx context.Context, p slack.FollowUpPost) error
	CheckFollowUp(target, origin string) error
}

// followUps delivers due follow-ups (followup.Firer) in serve: a message to its conversation (an
// API conversation's transcript, or the Slack gateway's thread or DM) or another target, and a
// prompt as a turn in its conversation in the scheduled runs' lane, its reply delivered as that
// conversation's replies are.
type followUps struct {
	store *sessions.Store
	srv   *api.Server
	slack atomic.Pointer[followUpPoster]
	logf  func(string)
	loc   *time.Location
}

func (f *followUps) poster() followUpPoster {
	if p := f.slack.Load(); p != nil {
		return *p
	}
	return nil
}

// errNoSlack is a follow-up for Slack with no gateway running (a restart without the tokens).
var errNoSlack = errors.New("Slack is not connected: the follow-up could not be posted")

// busy maps the API's slot errors to the queue's.
func busy(err error) error {
	switch {
	case errors.Is(err, api.ErrClosing):
		return followup.ErrInterrupted
	case errors.Is(err, api.ErrBusy), errors.Is(err, api.ErrSessionBusy):
		return followup.ErrBusy
	}
	return err
}

// Fire implements followup.Firer.
func (f *followUps) Fire(ctx context.Context, it followup.Item) error {
	switch it.Kind {
	case followup.KindMessage:
		return f.message(ctx, it)
	case followup.KindPrompt:
		return f.prompt(ctx, it)
	}
	return fmt.Errorf("%w: unknown kind %q", followup.ErrGone, it.Kind)
}

// message delivers a message follow-up. To its own conversation: an API conversation (the owner's
// app chat, a oneshot's) gets an assistant row in its transcript, which is the delivery; a Slack
// conversation gets the post in its thread or DM (through the gateway: before_send, then the
// messaging policy), and the same row, so the next turn knows it was said. Both while the
// conversation is held, never between a turn's rows. Elsewhere: the gateway, under the policy.
func (f *followUps) message(ctx context.Context, it followup.Item) error {
	if it.Target != followup.TargetOrigin {
		p := f.poster()
		if p == nil {
			return errNoSlack
		}
		return p.FollowUp(ctx, slack.FollowUpPost{Target: it.Target, Origin: it.Origin.Session, Kind: slack.FollowUpMessage, Text: it.Text})
	}
	release, err := f.srv.Hold(it.Origin.Session)
	if err != nil {
		return busy(err)
	}
	defer release()
	tip, ok := f.store.Tip(it.Origin.Session)
	if it.Origin.Source == slack.Source {
		p := f.poster()
		if p == nil {
			return errNoSlack
		}
		if err := p.FollowUp(ctx, slack.FollowUpPost{Origin: it.Origin.Session, Kind: slack.FollowUpMessage, Text: it.Text}); err != nil {
			return err
		}
		if ok {
			if err := f.appendReply(tip, it.Text); err != nil {
				f.logf(fmt.Sprintf("follow-up %s: posted, but not kept in the conversation: %v", it.ID, err))
			}
		}
		return nil
	}
	if !ok {
		return followup.ErrGone
	}
	return f.appendReply(tip, it.Text)
}

func (f *followUps) appendReply(tip, text string) error {
	if _, err := f.store.Append(tip, wire.Message{Role: "assistant", Content: text}); err != nil {
		if errors.Is(err, sessions.ErrNotFound) || errors.Is(err, sessions.ErrEnded) {
			return followup.ErrGone // deleted, or ended (a oneshot's, once the command exited)
		}
		return err
	}
	return f.store.Sync(tip)
}

// prompt runs a prompt follow-up's turn in its conversation, in the scheduled runs' lane (so it
// never starves chat), and delivers its reply as the conversation's replies are: an API
// conversation's is in its transcript already, a Slack one's is posted in its thread or DM; a
// [SILENT] reply posts nothing. With another target the reply goes there.
func (f *followUps) prompt(ctx context.Context, it followup.Item) error {
	root := it.Origin.Session
	if root == "" {
		return followup.ErrGone
	}
	if _, ok := f.store.Root(root); !ok && it.Origin.Source != slack.Source {
		return followup.ErrGone // a deleted API conversation is not made again
	}
	if tip, ok := f.store.Tip(root); ok {
		if h, _ := f.store.Get(tip); h.EndReason != "" {
			return followup.ErrGone // ended (a oneshot's, once the command exited): no turn continues it
		}
	}
	slot, err := f.srv.ReserveJob(root, wire.Session{Source: it.Origin.Source, Title: "Slack thread"})
	if err != nil {
		return busy(err)
	}
	defer slot.Release()
	res, err := slot.Turn(ctx, f.input(it), loop.Options{})
	if err != nil {
		return err
	}
	text := res.Text
	if sched.Silent(text) || strings.TrimSpace(text) == "" {
		f.logf(fmt.Sprintf("follow-up %s: the turn answered [SILENT]; nothing posted", it.ID))
		return nil
	}
	switch {
	case it.Target != followup.TargetOrigin:
		p := f.poster()
		if p == nil {
			return errNoSlack
		}
		return p.FollowUp(ctx, slack.FollowUpPost{Target: it.Target, Origin: root, Kind: slack.FollowUpMessage, Text: text})
	case it.Origin.Source == slack.Source:
		p := f.poster()
		if p == nil {
			return errNoSlack
		}
		return p.FollowUp(ctx, slack.FollowUpPost{Origin: root, Kind: slack.FollowUpReply, Text: text})
	}
	return nil // the turn's reply is the conversation's last row
}

// input is a prompt follow-up's turn input: what it is (the agent's own, not a new message from
// anyone), when it was made and for when, then the prompt.
func (f *followUps) input(it followup.Item) string {
	created, _ := time.Parse(time.RFC3339, it.Created)
	late := ""
	if it.Late {
		late = "; it is late: you were not running at its time"
	}
	where := "your reply is posted in this conversation"
	if it.Target != followup.TargetOrigin {
		where = "your reply is posted to " + it.Target
	}
	return fmt.Sprintf("[Follow-up %s that you scheduled on %s for %s%s. No one wrote this now: do what it says; %s (reply exactly [SILENT] to post nothing).]\n%s",
		it.ID, followup.Local(created, f.loc), followup.Local(it.DueAt(), f.loc), late, where, it.Prompt)
}

// newFollowUps is the follow-ups' deliverer in serve; the queue starts with it.
func newFollowUps(rt *runtime, srv *api.Server) *followUps {
	f := &followUps{store: rt.store, srv: srv, logf: func(s string) { rt.logger.Println(s) }, loc: rt.cfg.Location}
	if f.loc == nil {
		f.loc = time.UTC
	}
	return f
}

// setSlack lets the follow-ups reach Slack through the gateway (its policy, its before_send).
func (f *followUps) setSlack(p followUpPoster) {
	f.slack.Store(&p)
}

// followUpCheck is the policy check the queue runs on a follow-up's target, through the gateway.
func followUpCheck(p followUpPoster) func(target string, origin followup.Conversation) error {
	return func(target string, origin followup.Conversation) error {
		if target == followup.TargetOrigin {
			target = ""
		}
		return p.CheckFollowUp(target, origin.Session)
	}
}
