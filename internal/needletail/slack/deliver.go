package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// Deliver posts a scheduled run's output (sched.Deliverer): to the home channel (a target with
// no channel), a channel, or a thread in one; a user id is their DM. A job the agent made (not
// Trusted) reaches only what the messaging policy allows, as send_message does; a repo job's
// targets are the repo's. A [SILENT] output is never posted (the scheduler does not hand one
// over; checked again here). The output is posted as it is, with no "Cronjob Response" wrapper
// (Hermes' cron.wrap_response: false), and never broadcasts.
func (g *Gateway) Deliver(ctx context.Context, d sched.Delivery) error {
	if d.Target.Platform != "slack" {
		return fmt.Errorf("needletail does not deliver to %s", d.Target.Platform)
	}
	if strings.TrimSpace(d.Text) == "" || sched.Silent(d.Text) {
		return nil
	}
	channel, thread := d.Target.Channel, d.Target.Thread
	if !d.Trusted {
		if err := g.CheckTarget(d.Target); err != nil {
			return err
		}
	}
	if channel == "" {
		if g.o.HomeChannel == "" {
			return errors.New("no home channel: set the Slack channel's home_channel")
		}
		channel = g.o.HomeChannel
	}
	channel, err := g.channelFor(ctx, channel)
	if err != nil {
		return err
	}
	_, err = g.post(ctx, channel, thread, d.Text)
	return err
}

// CheckTarget holds a scheduled job's target to the messaging policy: what cronjob_manage
// checks when the agent sets deliver or failure_deliver, and Deliver for a job the agent made.
func (g *Gateway) CheckTarget(t sched.Target) error {
	m := messaging.Target{Platform: t.Platform, Channel: t.Channel, Thread: t.Thread}
	if u := userIDRe.FindStringSubmatch(t.Channel); u != nil {
		m = messaging.Target{Platform: t.Platform, User: u[1]}
	}
	_, err := g.Policy().Check(m, false)
	return err
}

var (
	channelIDRe = regexp.MustCompile(`^[CGD][A-Z0-9]{8,}$`)
	userIDRe    = regexp.MustCompile(`^<?@?([UW][A-Z0-9]{8,})>?$`)
)

// channelFor is a channel id as given, or a user's DM channel (conversations.open).
func (g *Gateway) channelFor(ctx context.Context, target string) (string, error) {
	if m := userIDRe.FindStringSubmatch(target); m != nil {
		return g.o.Web.OpenDM(ctx, m[1])
	}
	if !channelIDRe.MatchString(target) {
		return "", fmt.Errorf("%q is not a Slack channel or user id", target)
	}
	return target, nil
}

// Policy is the messaging policy as this gateway applies it: the home channel, the channels and
// threads the agent is engaged in, the DMs of the people allowed_users names (by id), and a
// broadcast only with the config's grant (slack.broadcast).
func (g *Gateway) Policy() messaging.Policy {
	users := []string{}
	for _, u := range g.o.AllowedUsers {
		if u != "*" {
			users = append(users, u)
		}
	}
	return messaging.Policy{Home: g.o.HomeChannel, AllowedUsers: users, Broadcast: g.o.Broadcast, Engaged: g.engaged}
}

// engaged is whether the agent is in a conversation: a channel it has a conversation in (thread
// ""), a DM it has its conversation in (any thread of it), or a thread it posted in, was
// mentioned in or has the session of.
func (g *Gateway) engaged(channel, thread string) bool {
	if strings.HasPrefix(channel, "D") {
		return g.o.Exists(DMSessionID(channel)) || slices.Contains(g.known(), channel)
	}
	if thread == "" {
		return slices.Contains(g.known(), channel)
	}
	return g.threads.has(channel+":"+thread) || g.o.Exists(SessionID(channel, thread))
}

// Send posts a message the agent sends on its own (send_message, Luau's send_message) under
// the messaging policy.
func (g *Gateway) Send(ctx context.Context, r messaging.Request) (messaging.Sent, error) {
	if strings.TrimSpace(r.Text) == "" {
		return messaging.Sent{}, errors.New("message is required")
	}
	t, err := messaging.Parse(r.Target)
	if err != nil {
		return messaging.Sent{}, err
	}
	broadcast, err := g.Policy().Check(t, r.Broadcast)
	if err != nil {
		return messaging.Sent{}, err
	}
	channel, thread := t.Channel, t.Thread
	switch {
	case t.User != "":
		if channel, err = g.o.Web.OpenDM(ctx, t.User); err != nil {
			return messaging.Sent{}, err
		}
	case channel == "":
		channel = g.o.HomeChannel
	}
	ts, err := g.postAs(ctx, channel, thread, r.Text, broadcast)
	if err != nil {
		return messaging.Sent{}, err
	}
	return messaging.Sent{Platform: "slack", Channel: channel, TS: ts, Home: channel == g.o.HomeChannel && thread == ""}, nil
}

// SendTool is Hermes' send_message for Slack, under the messaging policy (Send). The scheduler
// never offers it to a scheduled run (its reply is what is delivered).
func (g *Gateway) SendTool() tools.Tool {
	return tools.Tool{Name: messaging.SendTool, Description: sendDescription, Schema: json.RawMessage(sendSchema),
		Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (any, error) {
			var a struct {
				Action    string `json:"action"`
				Target    string `json:"target"`
				Message   string `json:"message"`
				Broadcast bool   `json:"broadcast"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("bad arguments: %v", err)
			}
			switch strings.ToLower(strings.TrimSpace(a.Action)) {
			case "list":
				return map[string]any{"targets": g.targets()}, nil
			case "", "send":
			default:
				return nil, fmt.Errorf("unknown action %q: send or list", a.Action)
			}
			sent, err := g.Send(ctx, messaging.Request{Target: a.Target, Text: a.Message, Broadcast: a.Broadcast})
			if err != nil {
				return nil, err
			}
			out := map[string]any{"success": true, "platform": sent.Platform, "chat_id": sent.Channel, "message_id": sent.TS}
			if sent.Home {
				out["note"] = fmt.Sprintf("Sent to slack home channel (chat_id: %s)", sent.Channel)
			}
			return out, nil
		}}
}

const sendDescription = `Send a message to Slack, or list the targets you may use.

Targets: 'slack' (the home channel), 'slack:<channel id>' for a channel you have a conversation in, 'slack:<channel id>:<thread ts>' for a thread you are in, or 'slack:<user id>' for an allowed user's DM. Call action 'list' to see them. Your reply in this conversation reaches it anyway: use this only to post somewhere else. @channel, @here, @everyone and user groups are never pinged unless you set broadcast and the agent is granted it.`

const sendSchema = `{"type":"object","properties":{
"action":{"type":"string","enum":["send","list"],"description":"send (default) or list the targets."},
"target":{"type":"string","description":"'slack' (home channel), 'slack:<channel>', 'slack:<channel>:<thread_ts>' or 'slack:<user id>'."},
"message":{"type":"string","description":"The message (Markdown)."},
"broadcast":{"type":"boolean","description":"Ping @channel, @here, @everyone or a user group the message names. Refused unless the agent is granted broadcasts; leave it out otherwise."}
}}`

func (g *Gateway) known() []string {
	if g.o.Channels == nil {
		return nil
	}
	return g.o.Channels()
}

func (g *Gateway) targets() []map[string]string {
	out := []map[string]string{}
	if g.o.HomeChannel != "" {
		out = append(out, map[string]string{"target": "slack", "description": "the home channel (" + g.o.HomeChannel + ")"})
	}
	for _, c := range g.known() {
		if c != g.o.HomeChannel {
			out = append(out, map[string]string{"target": "slack:" + c, "description": "a conversation you have had"})
		}
	}
	for _, u := range g.Policy().AllowedUsers {
		out = append(out, map[string]string{"target": "slack:" + u, "description": "a DM with an allowed user"})
	}
	return out
}

var _ sched.Deliverer = (*Gateway)(nil)
var _ messaging.Messenger = (*Gateway)(nil)
