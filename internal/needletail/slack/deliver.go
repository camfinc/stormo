package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// Deliver posts a scheduled run's output (sched.Deliverer): to the home channel (a target with
// no channel), a channel, or a thread in one; a user id is their DM. A [SILENT] output is never
// posted (the scheduler does not hand one over; checked again here). The output is posted as it
// is, with no "Cronjob Response" wrapper (Hermes' cron.wrap_response: false).
func (g *Gateway) Deliver(ctx context.Context, d sched.Delivery) error {
	if d.Target.Platform != "slack" {
		return fmt.Errorf("needletail does not deliver to %s", d.Target.Platform)
	}
	if strings.TrimSpace(d.Text) == "" || sched.Silent(d.Text) {
		return nil
	}
	channel, thread := d.Target.Channel, d.Target.Thread
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

// SendTool is Hermes' send_message for Slack: a message to the home channel, a channel the
// agent has conversations in, a thread in one, or an allowed user's DM. The scheduler never
// offers it to a scheduled run (its reply is what is delivered).
func (g *Gateway) SendTool() tools.Tool {
	return tools.Tool{Name: "send_message", Description: sendDescription, Schema: json.RawMessage(sendSchema),
		Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (any, error) {
			var a struct {
				Action  string `json:"action"`
				Target  string `json:"target"`
				Message string `json:"message"`
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
			if strings.TrimSpace(a.Message) == "" {
				return nil, errors.New("message is required")
			}
			channel, thread, home, err := g.resolve(ctx, a.Target)
			if err != nil {
				return nil, err
			}
			ts, err := g.post(ctx, channel, thread, a.Message)
			if err != nil {
				return nil, err
			}
			out := map[string]any{"success": true, "platform": "slack", "chat_id": channel, "message_id": ts}
			if home {
				out["note"] = fmt.Sprintf("Sent to slack home channel (chat_id: %s)", channel)
			}
			return out, nil
		}}
}

const sendDescription = `Send a message to Slack, or list the targets you may use.

Targets: 'slack' (the home channel), 'slack:<channel id>' or 'slack:<channel id>:<thread ts>' for a channel or thread you have talked in, or 'slack:<user id>' for an allowed user's DM. Call action 'list' to see them. Your reply in this conversation reaches it anyway: use this only to post somewhere else.`

const sendSchema = `{"type":"object","properties":{
"action":{"type":"string","enum":["send","list"],"description":"send (default) or list the targets."},
"target":{"type":"string","description":"'slack' (home channel), 'slack:<channel>', 'slack:<channel>:<thread_ts>' or 'slack:<user id>'."},
"message":{"type":"string","description":"The message (Markdown)."}
}}`

// resolve reads a send_message target: only the home channel, channels the agent has
// conversations in (or a thread in one), and allowed users' DMs.
func (g *Gateway) resolve(ctx context.Context, target string) (channel, thread string, home bool, err error) {
	t := strings.TrimSpace(target)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "slack:"), "Slack:")
	if t == "" || strings.EqualFold(t, "slack") || strings.EqualFold(t, "home") {
		if g.o.HomeChannel == "" {
			return "", "", false, errors.New("no home channel is set: name a channel")
		}
		return g.o.HomeChannel, "", true, nil
	}
	if m := userIDRe.FindStringSubmatch(t); m != nil {
		if !g.allowed(m[1]) {
			return "", "", false, fmt.Errorf("%s is not a user you may message", m[1])
		}
		ch, err := g.o.Web.OpenDM(ctx, m[1])
		return ch, "", false, err
	}
	channel, thread, _ = strings.Cut(t, ":")
	if !channelIDRe.MatchString(channel) {
		return "", "", false, fmt.Errorf("could not resolve %q: use a channel id (send_message action list shows the targets)", target)
	}
	if channel != g.o.HomeChannel && !slices.Contains(g.known(), channel) {
		return "", "", false, fmt.Errorf("%s is not a channel you have talked in (send_message action list shows the targets)", channel)
	}
	return channel, thread, channel == g.o.HomeChannel && thread == "", nil
}

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
	if !g.o.AllowAll {
		for _, u := range g.o.AllowedUsers {
			if u != "*" {
				out = append(out, map[string]string{"target": "slack:" + u, "description": "a DM with an allowed user"})
			}
		}
	}
	return out
}

var _ sched.Deliverer = (*Gateway)(nil)
