package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/approvals"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/slack"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// The messaging policy on Telegram (docs/needletail.md §4.14): what the agent sends on its own
// (send_message, Luau's send_message, its jobs' deliveries, its follow-ups) reaches the home chat
// (TELEGRAM_HOME_CHANNEL), a chat it has a conversation in, or the private chat of a person
// allowed_users names (a private chat's id is its person's user id). Nothing else; never a
// broadcast (Telegram has none).

// target is a chat (and topic) as a messaging target.
func (g *Gateway) target(chat int64, topic int) messaging.Target {
	t := messaging.Target{Platform: Source, Channel: strconv.FormatInt(chat, 10)}
	if topic != 0 {
		t.Thread = strconv.Itoa(topic)
	}
	return t
}

// Check is the messaging policy for t (a Telegram target).
func (g *Gateway) Check(t messaging.Target) error {
	if t.Platform != Source {
		return fmt.Errorf("%w: %s is not a Telegram target", messaging.ErrRefused, t)
	}
	chat := t.Channel
	switch {
	case chat == "" || chat == g.o.HomeChat:
		if g.o.HomeChat == "" {
			return fmt.Errorf("%w: no Telegram home chat is set (home_channel); name a chat", messaging.ErrRefused)
		}
		return nil
	case !strings.HasPrefix(chat, "-") && g.listed(chat):
		return nil // an allowed person's private chat (its id is theirs)
	case g.engaged(chat, t.Thread):
		return nil
	}
	return fmt.Errorf("%w: telegram:%s is not a chat you have a conversation in, the home chat or an allowed person's private chat", messaging.ErrRefused, chat)
}

// engaged: the agent has a conversation in the chat (a topic of it counts for the chat).
func (g *Gateway) engaged(chat, topic string) bool {
	id, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		return false
	}
	t, _ := strconv.Atoi(topic)
	if g.o.Exists(SessionID(id, t)) || g.o.Exists(SessionID(id, 0)) {
		return true
	}
	if g.o.Chats != nil {
		for _, s := range g.o.Chats() {
			if c, _, ok := Origin(s); ok && c == id {
				return true
			}
		}
	}
	return false
}

// chatOf is a target's chat id and topic (the home chat for none).
func (g *Gateway) chatOf(t messaging.Target) (int64, int, error) {
	chat := t.Channel
	if chat == "" {
		chat = g.o.HomeChat
	}
	id, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a Telegram chat id", chat)
	}
	topic, _ := strconv.Atoi(t.Thread)
	return id, topic, nil
}

// post sends text (the model's Markdown, redacted) to a chat: cut into parts within Telegram's
// bound, each as MarkdownV2, sent again as plain text when Telegram cannot parse it. It returns
// the first part's message id.
func (g *Gateway) post(ctx context.Context, chat int64, topic, replyTo int, text string) (int, error) {
	first := 0
	for i, part := range Split(g.o.Redact(text), MaxPart) {
		s := Send{Chat: chat, Thread: topic, Text: Format(part), ParseMode: "MarkdownV2"}
		if i == 0 {
			s.ReplyTo = replyTo
		}
		id, err := g.o.Bot.SendMessage(ctx, s)
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == 400 && strings.Contains(strings.ToLower(apiErr.Description), "parse") {
			s.Text, s.ParseMode = part, ""
			id, err = g.o.Bot.SendMessage(ctx, s)
		}
		if err != nil {
			return first, err
		}
		if first == 0 {
			first = id
		}
	}
	return first, nil
}

// say posts text in a conversation after the before_send handlers (a turn's reply, the runtime's
// note).
func (g *Gateway) say(ctx context.Context, kind, session string, chat int64, topic, replyTo int, text string) (int, error) {
	text, err := g.beforeSend(ctx, messaging.Outgoing{Kind: kind, Session: session, Text: text}, g.target(chat, topic), kind == messaging.KindNotice)
	if err != nil {
		return 0, err
	}
	return g.post(ctx, chat, topic, replyTo, text)
}

// beforeSend runs the agent's before_send handlers on a message about to go to target: the text
// to post, or ErrBlocked. A handler that fails blocks it (fails closed) but for the runtime's own
// notice; a rewrite changes only the text.
func (g *Gateway) beforeSend(ctx context.Context, o messaging.Outgoing, target messaging.Target, notice bool) (string, error) {
	if g.o.Handlers == nil || messaging.HandlerEvent(ctx) == "before_send" {
		return o.Text, nil
	}
	o.Platform, o.Target, o.Channel, o.ThreadTS = Source, target.String(), target.Channel, target.Thread
	o.Text = g.o.Redact(o.Text)
	source := o.Session
	if source == "" {
		source = messaging.SessionOf(ctx)
	}
	ctx = messaging.WithSession(ctx, source)
	if g.o.Untrusted != nil && g.o.Untrusted(source) {
		ctx = messaging.WithUntrusted(ctx, messaging.UntrustedConversation)
	}
	ctx = messaging.WithQuarantine(ctx, func(string) error {
		if source == "" || g.o.Quarantine == nil {
			return errors.New("durable before_send quarantine unavailable")
		}
		return g.o.Quarantine(source)
	})
	v := g.o.Handlers.BeforeSend(ctx, o)
	if v.Untrusted != "" {
		if source == "" || g.o.Quarantine == nil {
			return "", errors.New("marked before_send result requires durable conversation quarantine")
		}
		if err := g.o.Quarantine(source); err != nil {
			return "", err
		}
		if err := messaging.MarkUntrusted(ctx, v.Untrusted); err != nil {
			return "", err
		}
	}
	switch v.Action {
	case "block":
		if v.Failed && notice {
			g.logf("telegram: a %s to %s: the agent's handler %s failed; the runtime's notice is posted all the same", o.Kind, o.Target, v.By)
			return o.Text, nil
		}
		g.logf("telegram: a %s to %s was blocked by the agent's handler %s", o.Kind, o.Target, v.By)
		if v.Reason != "" {
			return "", fmt.Errorf("%w (%s: %s)", messaging.ErrBlocked, v.By, v.Reason)
		}
		return "", fmt.Errorf("%w (%s)", messaging.ErrBlocked, v.By)
	case "rewrite":
		return v.Text, nil
	}
	return o.Text, nil
}

// quarantineSend is the information-flow rule for a message the agent sends on its own from a
// tainted run: back to its own conversation it is a reply (allowed); anywhere else it needs a
// person's approval.
func (g *Gateway) quarantineSend(ctx context.Context, target messaging.Target) error {
	if messaging.Endorsed(ctx) != "" {
		return nil
	}
	session := messaging.SessionOf(ctx)
	why := messaging.UntrustedWhy(ctx)
	if why == "" && g.o.Untrusted != nil && g.o.Untrusted(session) {
		why = messaging.UntrustedConversation
	}
	if why == "" {
		return nil
	}
	root := session
	if g.o.Root != nil {
		if r, ok := g.o.Root(session); ok {
			root = r
		}
	}
	chat, topic, ok := Origin(root)
	dest, dtopic, err := g.chatOf(target)
	if ok && err == nil && dest == chat && dtopic == topic {
		return nil
	}
	return messaging.NeedsApproval("send_message to "+target.String()+" (outside this conversation)", why)
}

// Send posts a message the agent sends on its own (send_message, Luau's send_message) under the
// messaging policy.
func (g *Gateway) Send(ctx context.Context, r messaging.Request) (messaging.Sent, error) {
	ctx = messaging.WithQuarantine(ctx, nil)
	if strings.TrimSpace(r.Text) == "" {
		return messaging.Sent{}, errors.New("message is required")
	}
	t, err := messaging.Parse(r.Target)
	if err != nil {
		return messaging.Sent{}, err
	}
	if t.Platform != Source {
		return messaging.Sent{}, fmt.Errorf("%w: %s is not a Telegram target", messaging.ErrRefused, r.Target)
	}
	if r.Broadcast {
		return messaging.Sent{}, fmt.Errorf("%w: Telegram has no broadcast", messaging.ErrRefused)
	}
	gate := g.quarantineSend(ctx, t)
	if _, ok := messaging.AsApproval(gate); !ok && gate != nil {
		return messaging.Sent{}, gate
	}
	text := r.Text
	if !messaging.Filtered(ctx) {
		if text, err = g.beforeSend(messaging.WithoutEndorsed(ctx), messaging.Outgoing{Kind: messaging.KindSend, Session: messaging.SessionOf(ctx), Text: r.Text}, t, false); err != nil {
			return messaging.Sent{}, err
		}
	}
	if messaging.Endorsed(ctx) != "" && !messaging.Filtered(ctx) && text != g.o.Redact(r.Text) {
		return messaging.Sent{}, fmt.Errorf("%w: the agent's before_send handlers would change the message the owner approved; it is not sent altered", messaging.ErrBlocked)
	}
	if need, ok := messaging.AsApproval(gate); ok {
		need.Payload, _ = json.Marshal(map[string]any{"target": r.Target, "message": text})
		need.Filtered = true
		return messaging.Sent{}, need
	}
	if err := g.Check(t); err != nil {
		return messaging.Sent{}, err
	}
	chat, topic, err := g.chatOf(t)
	if err != nil {
		return messaging.Sent{}, err
	}
	id, err := g.post(ctx, chat, topic, 0, text)
	if err != nil {
		return messaging.Sent{}, err
	}
	return messaging.Sent{Platform: Source, Channel: strconv.FormatInt(chat, 10), TS: strconv.Itoa(id), Home: t.Channel == "" || t.Channel == g.o.HomeChat}, nil
}

// Targets are the Telegram targets send_message's list shows.
func (g *Gateway) Targets() []map[string]string {
	out := []map[string]string{}
	if g.o.HomeChat != "" {
		out = append(out, map[string]string{"target": "telegram", "description": "the Telegram home chat (" + g.o.HomeChat + ")"})
	}
	seen := map[string]bool{g.o.HomeChat: true}
	if g.o.Chats != nil {
		for _, s := range g.o.Chats() {
			if c, topic, ok := Origin(s); ok {
				t := g.target(c, topic).String()
				if !seen[t] && t != "telegram:"+g.o.HomeChat {
					seen[t] = true
					out = append(out, map[string]string{"target": t, "description": "a Telegram chat you have a conversation in"})
				}
			}
		}
	}
	for _, u := range g.o.AllowedUsers {
		if u != "*" && !seen["telegram:"+u] {
			seen["telegram:"+u] = true
			out = append(out, map[string]string{"target": "telegram:" + u, "description": "the private chat of an allowed Telegram user"})
		}
	}
	return out
}

// SendTool is send_message for an agent whose only chat is Telegram (with Slack, Slack's tool
// takes Telegram targets too).
func (g *Gateway) SendTool() tools.Tool {
	return tools.Tool{Name: messaging.SendTool, Schema: json.RawMessage(sendSchema),
		Description: "Send a message to a Telegram chat, or list the targets you may use. Targets: 'telegram' (the home chat), 'telegram:<chat id>' " +
			"(a chat you have a conversation in, or an allowed person's private chat). Your reply in this conversation reaches it anyway: use this " +
			"only to post somewhere else. Telegram has no broadcast.",
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
				return map[string]any{"targets": g.Targets()}, nil
			case "", "send":
			default:
				return nil, fmt.Errorf("unknown action %q: send or list", a.Action)
			}
			if env != nil {
				ctx = messaging.WithSession(ctx, env.Session)
				if why := tools.UntrustedWhy(ctx, env); why != "" {
					ctx = messaging.WithUntrusted(ctx, why)
				}
			}
			sent, err := g.Send(ctx, messaging.Request{Target: a.Target, Text: a.Message})
			if err != nil {
				return nil, err
			}
			return map[string]any{"success": true, "platform": sent.Platform, "chat_id": sent.Channel, "message_id": sent.TS}, nil
		}}
}

const sendSchema = `{"type":"object","properties":{
"action":{"type":"string","enum":["send","list"],"description":"send (default) or list known targets."},
"target":{"type":"string","description":"'telegram' (home chat) or 'telegram:<chat id>'."},
"message":{"type":"string","description":"The message (Markdown)."}
}}`

var _ messaging.Messenger = (*Gateway)(nil)
var _ sched.Deliverer = (*Gateway)(nil)

// CheckTarget holds a scheduled job's Telegram target to the messaging policy.
func (g *Gateway) CheckTarget(t sched.Target) error {
	return g.Check(messaging.Target{Platform: t.Platform, Channel: t.Channel, Thread: t.Thread})
}

// Deliver posts a scheduled run's output to a Telegram target: the home chat (a target with no
// chat) or a chat. A job the agent made reaches only what the messaging policy allows; a repo
// job's targets are the repo's. Never [SILENT], never a broadcast.
func (g *Gateway) Deliver(ctx context.Context, d sched.Delivery) error {
	ctx = messaging.WithQuarantine(ctx, nil)
	if why := g.deliveryQuarantine(ctx, d.Session); why != "" {
		return messaging.Deferred("scheduled Telegram delivery", why)
	}
	if d.Target.Platform != Source {
		return fmt.Errorf("%w: the Telegram gateway does not deliver to %s", sched.ErrUnsupportedPlatform, d.Target.Platform)
	}
	if strings.TrimSpace(d.Text) == "" || sched.Silent(d.Text) {
		return nil
	}
	mt := messaging.Target{Platform: Source, Channel: d.Target.Channel, Thread: d.Target.Thread}
	text, err := g.beforeSend(messaging.WithoutEndorsed(ctx), messaging.Outgoing{Kind: messaging.KindDelivery, Session: d.Session, Text: d.Text}, mt, d.Failure)
	if err != nil {
		return err
	}
	if messaging.Endorsed(ctx) != "" && text != g.o.Redact(d.Text) {
		return fmt.Errorf("%w: the agent's before_send handlers would change the report the owner approved; it is not delivered altered", messaging.ErrBlocked)
	}
	if !d.Trusted {
		if err := g.Check(mt); err != nil {
			return err
		}
	} else if d.Target.Channel == "" && g.o.HomeChat == "" {
		return errors.New("no Telegram home chat: set the Telegram channel's home_channel")
	}
	chat, topic, err := g.chatOf(mt)
	if err != nil {
		return err
	}
	_, err = g.post(ctx, chat, topic, 0, text)
	return err
}

func (g *Gateway) deliveryQuarantine(ctx context.Context, session string) string {
	if messaging.Endorsed(ctx) != "" {
		return ""
	}
	if why := messaging.UntrustedWhy(ctx); why != "" {
		return why
	}
	if g.o.Untrusted != nil && g.o.Untrusted(session) {
		return messaging.UntrustedConversation
	}
	return ""
}

// AlertDeliveries keeps the one home notice about kept reports (sched.DeliveryAlerter) in the
// Telegram home chat, for an agent whose only chat is Telegram: posted, then edited in place.
func (g *Gateway) AlertDeliveries(ctx context.Context, a sched.DeliveryAlert) (sched.AlertRef, error) {
	if g.o.HomeChat == "" {
		return sched.AlertRef{}, errors.New("no Telegram home chat: cannot alert about scheduled delivery failure")
	}
	text := g.o.Redact(alertText(a.JobIDs))
	if a.Ref.TS != "" && a.Ref.Channel != "" {
		chat, _ := strconv.ParseInt(a.Ref.Channel, 10, 64)
		id, _ := strconv.Atoi(a.Ref.TS)
		if err := g.o.Bot.EditMessage(ctx, chat, id, text); err == nil {
			return a.Ref, nil
		} else if len(a.JobIDs) == 0 {
			return sched.AlertRef{}, err
		}
	}
	if len(a.JobIDs) == 0 {
		return sched.AlertRef{}, nil
	}
	chat, err := strconv.ParseInt(g.o.HomeChat, 10, 64)
	if err != nil {
		return sched.AlertRef{}, err
	}
	id, err := g.o.Bot.SendMessage(ctx, Send{Chat: chat, Text: text})
	if err != nil {
		return sched.AlertRef{}, err
	}
	return sched.AlertRef{Channel: g.o.HomeChat, TS: strconv.Itoa(id)}, nil
}

func alertText(jobs []string) string {
	if len(jobs) == 0 {
		return "Scheduled report delivery recovered: no kept report is waiting for review now."
	}
	n := "1 scheduled report"
	verb := "is"
	if len(jobs) > 1 {
		n, verb = fmt.Sprintf("%d scheduled reports", len(jobs)), "are"
	}
	return fmt.Sprintf("%s could not be delivered and %s kept for review (jobs %s); further runs of those jobs are on hold. "+
		"Please confirm where each should go and that I have access, so I can retry it (or discard it).", n, verb, strings.Join(jobs, ", "))
}

// FollowUp posts a follow-up (docs/needletail.md §4.12): in its own Telegram conversation as the
// turn's reply would be; anywhere else under the messaging policy. Who asked must still be
// admitted.
func (g *Gateway) FollowUp(ctx context.Context, p slack.FollowUpPost) error {
	ctx = messaging.WithQuarantine(ctx, nil)
	ctx = messaging.WithSession(ctx, p.Origin)
	untrusted := p.Untrusted || g.o.Untrusted != nil && g.o.Untrusted(p.Origin)
	if untrusted {
		ctx = messaging.WithUntrusted(ctx, messaging.UntrustedConversation)
	}
	if strings.TrimSpace(p.Text) == "" {
		return nil
	}
	if err := g.admits(p.Users); err != nil {
		return err
	}
	var t messaging.Target
	if p.Target == "" {
		chat, topic, ok := Origin(p.Origin)
		if !ok {
			return fmt.Errorf("%s is not a Telegram conversation", p.Origin)
		}
		t = g.target(chat, topic)
	} else {
		var err error
		if t, err = messaging.Parse(p.Target); err != nil {
			return err
		}
		if err := g.Check(t); err != nil {
			return err
		}
	}
	text, err := g.beforeSend(ctx, messaging.Outgoing{Kind: p.Kind, Session: p.Origin, Text: p.Text}, t, false)
	if err != nil {
		return err
	}
	chat, topic, err := g.chatOf(t)
	if err != nil {
		return err
	}
	if untrusted || messaging.UntrustedWhy(ctx) != "" {
		// marked text: its destination conversation is created and marked before it is posted
		if g.o.Quarantine == nil {
			return errors.New("durable Telegram destination quarantine is unavailable")
		}
		if err := g.o.Quarantine(SessionID(chat, topic)); err != nil {
			return err
		}
	}
	_, err = g.post(ctx, chat, topic, 0, text)
	return err
}

// CheckFollowUp is the messaging policy for a follow-up's target ("" its own conversation) and
// the admission of who asked.
func (g *Gateway) CheckFollowUp(target, origin string, users []string) error {
	if err := g.admits(users); err != nil {
		return err
	}
	if target == "" {
		if _, _, ok := Origin(origin); !ok {
			return errors.New("this is not a Telegram conversation")
		}
		return nil
	}
	t, err := messaging.Parse(target)
	if err != nil {
		return err
	}
	return g.Check(t)
}

// admits: everyone who asked (telegram:<id>) may still talk to the agent.
func (g *Gateway) admits(users []string) error {
	for _, u := range users {
		id := strings.TrimPrefix(u, "telegram:")
		if !g.admitted(id) {
			return fmt.Errorf("%w: %s, who asked for it, may no longer talk to the agent (allowed_users)", messaging.ErrRefused, u)
		}
	}
	return nil
}

// Tell posts an approval's outcome in the Telegram conversation it was asked in (the runtime's
// notice; never the payload).
func (g *Gateway) Tell(ctx context.Context, r *approvals.Request) error {
	chat, topic, ok := Origin(r.Conversation)
	if !ok {
		return nil
	}
	_, err := g.say(ctx, messaging.KindNotice, r.Conversation, chat, topic, 0, slack.OutcomeNotice(r))
	return err
}

// Listed is whether allowed_users names a Telegram user id (verified).
func (g *Gateway) Listed(id string) bool { return slices.Contains(g.o.AllowedUsers, id) }
