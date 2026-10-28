package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/api"
	"github.com/camfinc/stormo/internal/needletail/approvals"
	"github.com/camfinc/stormo/internal/needletail/luau"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/secrets"
	"github.com/camfinc/stormo/internal/needletail/slack"
	"github.com/camfinc/stormo/internal/needletail/telegram"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// platforms are the chat gateways serve runs (Slack, Telegram; either may be nil) as one: the
// scheduler's deliverer and target check, Luau's messenger, the follow-ups' poster, the approvals'
// notifier. Each target goes to its platform's gateway, which applies its own messaging policy.
// A Slack home target ("slack", "home", "all") goes to Telegram's home chat when only Telegram
// runs.
type platforms struct {
	slack    *slack.Gateway
	telegram *telegram.Gateway
}

// errNoGateway is a target whose platform has no gateway running here.
func errNoGateway(platform string) error {
	return fmt.Errorf("%w: no %s gateway runs here", messaging.ErrRefused, platform)
}

// route is the platform a scheduled target goes to (a Slack home target to Telegram's when only
// Telegram runs).
func (p *platforms) route(t sched.Target) sched.Target {
	if t.Platform == "slack" && t.Channel == "" && p.slack == nil && p.telegram != nil {
		return sched.Target{Platform: telegram.Source}
	}
	return t
}

// Deliver implements sched.Deliverer.
func (p *platforms) Deliver(ctx context.Context, d sched.Delivery) error {
	d.Target = p.route(d.Target)
	switch {
	case d.Target.Platform == "slack" && p.slack != nil:
		return p.slack.Deliver(ctx, d)
	case d.Target.Platform == telegram.Source && p.telegram != nil:
		return p.telegram.Deliver(ctx, d)
	case d.Target.Platform == "slack" || d.Target.Platform == telegram.Source:
		return sched.Pending{}.Deliver(ctx, d)
	}
	return fmt.Errorf("%w: needletail does not deliver to %s", sched.ErrUnsupportedPlatform, d.Target.Platform)
}

// CheckTarget is the messaging policy for a job's target, by its platform.
func (p *platforms) CheckTarget(t sched.Target) error {
	t = p.route(t)
	switch {
	case t.Platform == "slack" && p.slack != nil:
		return p.slack.CheckTarget(t)
	case t.Platform == telegram.Source && p.telegram != nil:
		return p.telegram.CheckTarget(t)
	}
	return errNoGateway(t.Platform)
}

// AlertDeliveries keeps the home notice about kept reports: Slack's home channel, else Telegram's
// home chat.
func (p *platforms) AlertDeliveries(ctx context.Context, a sched.DeliveryAlert) (sched.AlertRef, error) {
	if p.slack != nil {
		return p.slack.AlertDeliveries(ctx, a)
	}
	if p.telegram != nil {
		return p.telegram.AlertDeliveries(ctx, a)
	}
	return sched.AlertRef{}, errors.New("no gateway runs here")
}

// Send implements messaging.Messenger (Luau's send_message; Slack's tool hands a Telegram target
// here too).
func (p *platforms) Send(ctx context.Context, r messaging.Request) (messaging.Sent, error) {
	t, err := messaging.Parse(r.Target)
	if err != nil {
		return messaging.Sent{}, err
	}
	switch {
	case t.Platform == "slack" && p.slack != nil:
		return p.slack.Send(ctx, r)
	case t.Platform == telegram.Source && p.telegram != nil:
		return p.telegram.Send(ctx, r)
	}
	return messaging.Sent{}, errNoGateway(t.Platform)
}

// platformOf is a follow-up's platform: its target's, else its conversation's.
func platformOf(target, origin string) string {
	if target != "" {
		if t, err := messaging.Parse(target); err == nil {
			return t.Platform
		}
		return ""
	}
	if strings.HasPrefix(origin, "telegram-") {
		return telegram.Source
	}
	return "slack"
}

// FollowUp posts a follow-up through its platform's gateway.
func (p *platforms) FollowUp(ctx context.Context, post slack.FollowUpPost) error {
	switch platformOf(post.Target, post.Origin) {
	case "slack":
		if p.slack != nil {
			return p.slack.FollowUp(ctx, post)
		}
		return errNoSlack
	case telegram.Source:
		if p.telegram != nil {
			return p.telegram.FollowUp(ctx, post)
		}
		return errors.New("Telegram is not connected: the follow-up could not be posted")
	}
	return fmt.Errorf("%w: %s", messaging.ErrRefused, post.Target)
}

// CheckFollowUp is the policy for a follow-up's target, through its platform's gateway.
func (p *platforms) CheckFollowUp(target, origin string, users []string) error {
	switch platformOf(target, origin) {
	case "slack":
		if p.slack != nil {
			return p.slack.CheckFollowUp(target, origin, users)
		}
	case telegram.Source:
		if p.telegram != nil {
			return p.telegram.CheckFollowUp(target, origin, users)
		}
	}
	return errNoGateway(platformOf(target, origin))
}

// Ask, Update and Tell are the approvals' notifier: cards go to the approvers' Slack DMs; the
// outcome is told in the conversation it was asked in, on its platform.
func (p *platforms) Ask(ctx context.Context, r *approvals.Request, approver string) (approvals.Posted, error) {
	if p.slack == nil {
		return approvals.Posted{}, errors.New("approval cards need the Slack gateway")
	}
	return p.slack.Ask(ctx, r, approver)
}

func (p *platforms) Update(ctx context.Context, r *approvals.Request, posted approvals.Posted) error {
	if p.slack == nil {
		return errors.New("approval cards need the Slack gateway")
	}
	return p.slack.Update(ctx, r, posted)
}

func (p *platforms) Tell(ctx context.Context, r *approvals.Request) error {
	if strings.HasPrefix(r.Conversation, "telegram-") {
		if p.telegram == nil {
			return errors.New("Telegram is not connected")
		}
		return p.telegram.Tell(ctx, r)
	}
	if p.slack == nil {
		return nil
	}
	return p.slack.Tell(ctx, r)
}

// states are the gateways' states (the activity's platforms).
func (p *platforms) states() map[string]wire.Platform {
	out := map[string]wire.Platform{}
	if p.slack != nil {
		out["slack"] = p.slack.Platform()
	}
	if p.telegram != nil {
		out[telegram.Source] = p.telegram.Platform()
	}
	return out
}

// TelegramAPIEnv overrides Telegram's Bot API URL; tests only: it must be a loopback address.
const TelegramAPIEnv = "NEEDLETAIL_TELEGRAM_API_URL"

// startTelegram is the Telegram gateway (§4.14), when needletail.yaml names the channel and
// TELEGRAM_BOT_TOKEN is set; nil otherwise. Its settings are the channel's, in Hermes'
// TELEGRAM_* env.
func startTelegram(rt *runtime, srv *api.Server) *telegram.Gateway {
	if !slices.Contains(rt.cfg.Channels, telegram.Source) {
		return nil
	}
	token := rt.sec.Get(secrets.TelegramBot)
	if token == "" {
		rt.logger.Printf("telegram: the agent has a Telegram channel but %s is missing: the gateway is off", secrets.TelegramBot)
		return nil
	}
	store := rt.store
	create := func(session string, untrusted bool) error {
		if _, ok := store.Root(session); ok {
			return nil
		}
		if _, err := store.Create(wire.Session{ID: session, Source: telegram.Source, Title: "Telegram chat", Untrusted: untrusted}); err != nil {
			if _, exists := store.Root(session); !exists {
				return err
			}
		}
		return nil
	}
	o := telegram.Options{
		Bot:       &telegram.Bot{BaseURL: os.Getenv(TelegramAPIEnv), Token: token},
		Turns:     slackTurns{srv},
		Exists:    func(session string) bool { _, ok := store.Root(session); return ok },
		Root:      store.Root,
		Untrusted: store.Untrusted,
		Quarantine: func(session string) error {
			if err := create(session, true); err != nil {
				return err
			}
			return store.MarkUntrusted(session)
		},
		Chats:        func() []string { return telegramChats(rt) },
		AllowedUsers: envList("TELEGRAM_ALLOWED_USERS"),
		AllowAll:     strings.EqualFold(os.Getenv("TELEGRAM_ALLOW_ALL_USERS"), "true"),
		HomeChat:     strings.TrimSpace(os.Getenv("TELEGRAM_HOME_CHANNEL")),
		Redact:       rt.red.Redact,
		Log:          func(s string) { rt.logger.Println(s) },
		Files:        rt.files,
		STT:          startSTT(rt),
		Sees:         func() bool { return rt.agent.LLM.Sees(0) },
	}
	if rt.luau != nil {
		o.Handlers = luau.Events{RT: rt.luau} // the agent's on_message, on_mention and before_send (§5.5)
	}
	gw, err := telegram.New(o)
	if err != nil {
		rt.logger.Printf("telegram: %v: the gateway is off", err)
		return nil
	}
	rt.agent.Sections = append(rt.agent.Sections, gw.Guidance)
	rt.guide.Telegram = true
	rt.setGuide()
	gw.Start()
	return gw
}

// telegramChats are the agent's Telegram conversations (their session ids).
func telegramChats(rt *runtime) []string {
	out := []string{}
	for _, s := range rt.store.List(1000) {
		id := s.ID
		if s.Root != "" {
			id = s.Root
		}
		if _, _, ok := telegram.Origin(id); ok && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// envList is a comma-separated env value as a list.
func envList(name string) []string {
	out := []string{}
	for _, u := range strings.Split(os.Getenv(name), ",") {
		if u = strings.TrimSpace(u); u != "" {
			out = append(out, u)
		}
	}
	return out
}

// wirePlatforms makes the running gateways one for everything that sends on the agent's behalf:
// the scheduler, Luau, the follow-ups, and send_message (Slack's tool, taking Telegram targets
// too; Telegram's own when Slack is off).
func wirePlatforms(rt *runtime, p *platforms, fu *followUps) {
	if p.slack == nil && p.telegram == nil {
		return
	}
	rt.sched.SetDeliverer(p)
	rt.sched.SetTargetCheck(p.CheckTarget)
	fu.setSlack(p)
	rt.followUps.SetTargetCheck(followUpCheck(p))
	if rt.luau != nil {
		rt.luau.Messenger = p
	}
	if p.slack != nil && p.telegram != nil {
		p.slack.SetElsewhere(p, p.telegram.Targets)
	}
	if p.slack == nil {
		rt.agent.Tools.Add(p.telegram.SendTool())
	}
}
