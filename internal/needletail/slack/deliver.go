package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

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
	ctx = messaging.WithQuarantine(ctx, nil)
	if why := g.deliveryQuarantine(ctx, d.Session); why != "" {
		return messaging.Deferred("scheduled Slack delivery", why)
	}
	if d.Target.Platform != "slack" {
		return fmt.Errorf("needletail does not deliver to %s", d.Target.Platform)
	}
	if strings.TrimSpace(d.Text) == "" || sched.Silent(d.Text) {
		return nil
	}
	channel, thread := d.Target.Channel, d.Target.Thread
	// the agent's before_send handlers first; the messaging policy decides after them
	mt := messaging.Target{Platform: "slack", Channel: channel, Thread: thread}
	if u := userIDRe.FindStringSubmatch(channel); u != nil {
		mt = messaging.Target{Platform: "slack", User: u[1]}
	}
	// a failure (or auto-pause) notice is the runtime's: a broken handler does not keep it back
	text, err := g.beforeSendAs(ctx, messaging.Outgoing{Kind: messaging.KindDelivery, Session: d.Session, Text: d.Text}, mt, d.Failure)
	if err != nil {
		return err
	}
	if why := g.deliveryQuarantine(ctx, d.Session); why != "" {
		return messaging.Deferred("scheduled Slack delivery", why)
	}
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
	channel, err = g.channelFor(ctx, channel)
	if err != nil {
		return err
	}
	_, err = g.post(ctx, channel, thread, text)
	return err
}

// deliveryFailureAlertText contains no job, destination, result or error material.
const deliveryFailureAlertText = "A scheduled report could not be delivered. I have kept the report and put further runs of that job on hold. Please confirm where it should be sent and that I have access, so I can retry the saved report."

var _ sched.DeliveryAlerter = (*Gateway)(nil)

// AlertDelivery is a deterministic runtime notice, never model/handler output. It
// bypasses before_send and the general send surface, reaches only the configured
// home, and g.post always formats without broadcasts. Missing home/API failures
// remain errors so the scheduler can retain the pending alert for retry.
func (g *Gateway) AlertDelivery(ctx context.Context) error {
	if g.o.HomeChannel == "" {
		return errors.New("no home channel: cannot alert about scheduled delivery failure")
	}
	channel, err := g.channelFor(ctx, g.o.HomeChannel)
	if err != nil {
		return err
	}
	_, err = g.post(ctx, channel, "", deliveryFailureAlertText)
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
// threads the agent is engaged in, the DMs of the people allowed_users names (by id: a DM channel
// is theirs only when its person is one of them), and a broadcast only with the config's grant
// (slack.broadcast).
func (g *Gateway) Policy() messaging.Policy {
	users := []string{}
	for _, u := range g.o.AllowedUsers {
		if u != "*" {
			users = append(users, u)
		}
	}
	return messaging.Policy{Home: g.o.HomeChannel, AllowedUsers: users, Broadcast: g.o.Broadcast, Engaged: g.engaged, DMUser: g.dmUser}
}

// dmUser is the person of a DM channel: the author the gateway saw write there, else Slack's
// conversations.info (cached).
func (g *Gateway) dmUser(channel string) (string, error) {
	g.mu.Lock()
	u, ok := g.dmUsers[channel]
	g.mu.Unlock()
	if ok {
		return u, nil
	}
	ctx, cancel := context.WithTimeout(g.base, 10*time.Second)
	defer cancel()
	u, err := g.o.Web.DMUser(ctx, channel)
	if err != nil {
		return "", err
	}
	g.rememberDM(channel, u)
	return u, nil
}

// KnownDMUser is the person of a DM channel as the gateway knows it now (the author it saw write
// there, or a lookup it made), without asking Slack: "" when it does not know. The memory's
// audience of a DM turn (the gateway learns the person from the message before the turn runs).
func (g *Gateway) KnownDMUser(channel string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.dmUsers[channel]
}

func (g *Gateway) rememberDM(channel, user string) {
	if user == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.dmUsers) >= 5000 {
		g.dmUsers = map[string]string{}
	}
	g.dmUsers[channel] = user
}

// engaged is whether the agent is in a conversation: a channel it has a conversation in (thread
// ""), or a thread it posted in, was mentioned in or has the session of. (A DM channel is never
// asked: the policy decides it by its person, dmUser.)
func (g *Gateway) engaged(channel, thread string) bool {
	if thread == "" {
		return slices.Contains(g.known(), channel)
	}
	return g.threads.has(channel+":"+thread) || g.o.Exists(SessionID(channel, thread))
}

// Send posts a message the agent sends on its own (send_message, Luau's send_message) under
// the messaging policy.
func (g *Gateway) Send(ctx context.Context, r messaging.Request) (messaging.Sent, error) {
	ctx = messaging.WithQuarantine(ctx, nil)
	if strings.TrimSpace(r.Text) == "" {
		return messaging.Sent{}, errors.New("message is required")
	}
	t, err := messaging.Parse(r.Target)
	if err != nil {
		return messaging.Sent{}, err
	}
	if err := g.quarantineSend(ctx, t, r.Broadcast); err != nil {
		return messaging.Sent{}, err
	}
	// the agent's before_send handlers first (they change only the text); the messaging policy
	// decides after them
	text, err := g.beforeSend(ctx, messaging.Outgoing{Kind: messaging.KindSend, Session: messaging.SessionOf(ctx), Text: r.Text, Broadcast: r.Broadcast}, t)
	if err != nil {
		return messaging.Sent{}, err
	}
	if err := g.quarantineSend(ctx, t, r.Broadcast); err != nil {
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
	ts, err := g.postAs(ctx, channel, thread, text, broadcast)
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
				Action      string `json:"action"`
				Query       string `json:"query"`
				Cursor      string `json:"cursor"`
				ChannelType string `json:"channel_type"`
				Target      string `json:"target"`
				Message     string `json:"message"`
				Broadcast   bool   `json:"broadcast"`
			}
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("bad arguments: %v", err)
			}
			switch strings.ToLower(strings.TrimSpace(a.Action)) {
			case "channels":
				return g.discover(ctx, a.Query, a.Cursor, a.ChannelType)
			case "channel_info":
				return g.inspectChannel(ctx, a.Target)
			case "list":
				return map[string]any{"targets": g.targets()}, nil
			case "", "send":
			default:
				return nil, fmt.Errorf("unknown action %q: send, list, channels or channel_info", a.Action)
			}
			if env != nil {
				ctx = messaging.WithSession(ctx, env.Session)
				if why := tools.UntrustedWhy(ctx, env); why != "" {
					ctx = messaging.WithUntrusted(ctx, why)
				}
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

Targets: 'slack' (the home channel), 'slack:<channel id>' for a channel you have a conversation in, 'slack:<channel id>:<thread ts>' for a thread you are in, or 'slack:<user id>' for an allowed user's DM. Call action 'list' to see known targets. Use 'channels' to discover Slack-visible channel IDs and names (query filters one page; follow next_cursor until has_more is false), then 'channel_info' with the exact target ID to check current metadata and runtime policy. Public channels are the default; channel_type private_channel discovers private channels visible to the bot. Discovery never joins channels or grants permission to send; metadata is not a delivery receipt. Your reply in this conversation reaches it anyway: use this only to post somewhere else. @channel, @here, @everyone and user groups are never pinged unless you set broadcast and the agent is granted it.`

const sendSchema = `{"type":"object","properties":{
"action":{"type":"string","enum":["send","list","channels","channel_info"],"description":"send (default), list known targets, channels to discover channels, or channel_info to check an exact channel ID."},
"target":{"type":"string","description":"'slack' (home channel), 'slack:<channel>', 'slack:<channel>:<thread_ts>' or 'slack:<user id>'."},
"query":{"type":"string","description":"channels: case-insensitive channel-name substring, optionally starting with #; filters this page only."},
"cursor":{"type":"string","description":"channels: next_cursor from the preceding page; continue even if there were no matches."},
"channel_type":{"type":"string","enum":["public_channel","private_channel"],"description":"channels: public_channel by default; private_channel only lists channels visible to the bot."},
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

// quarantineSend is the information-flow rule for a message the agent sends on its own from a
// tainted run (docs/needletail.md §7): back to its own conversation (the runtime-known origin) it
// is a reply, allowed; anywhere else it needs a person's approval; a broadcast is refused.
func (g *Gateway) quarantineSend(ctx context.Context, target messaging.Target, broadcast bool) error {
	if messaging.Endorsed(ctx) != "" {
		return nil // a send a person approved: the approval endorses it
	}
	session := messaging.SessionOf(ctx)
	why := messaging.UntrustedWhy(ctx)
	if why == "" && g.o.Untrusted != nil && g.o.Untrusted(session) {
		why = messaging.UntrustedConversation
	}
	if why == "" {
		return nil
	}
	if broadcast {
		return messaging.Deferred("a broadcast", why)
	}
	elsewhere := messaging.NeedsApproval("send_message to "+target.String()+" (outside this conversation)", why)
	channel, thread, ok := Origin(g.root(session))
	if !ok {
		return elsewhere
	}
	dest := target.Channel
	if strings.HasPrefix(channel, "D") {
		if in, ok := messaging.InputOf(ctx); ok {
			thread = in.Thread
		}
	}
	if target.User != "" {
		if !strings.HasPrefix(channel, "D") {
			return elsewhere
		}
		person, err := g.dmUser(channel)
		if err != nil || person != target.User {
			return elsewhere
		}
		dest = channel
	} else if dest == "" {
		dest = g.o.HomeChannel
	}
	if dest != channel || target.Thread != thread {
		return elsewhere
	}
	return nil
}

func (g *Gateway) deliveryQuarantine(ctx context.Context, session string) string {
	if messaging.Endorsed(ctx) != "" {
		return "" // a held report a person approved
	}
	if why := messaging.UntrustedWhy(ctx); why != "" {
		return why
	}
	if g.o.Untrusted != nil && g.o.Untrusted(session) {
		return messaging.UntrustedConversation
	}
	return ""
}
