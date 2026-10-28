package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/approvals"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// Approvals (docs/needletail.md §7): the gateway shows a request's card in each approver's DM
// (never in the tainted conversation), takes the buttons clicked on it over Socket Mode (the
// approver is the payload's user, as Slack's authenticated socket gives it; a typed "yes" never
// counts) and hands the decision to the approvals service, which runs exactly what the card
// carries. Then the card is updated in place and the outcome told in the conversation.

// Deciders are the approvals service as the gateway uses it.
type Decider interface {
	Decide(ctx context.Context, d approvals.Decision) (*approvals.Request, error)
	Get(id string) (*approvals.Request, bool)
}

// Button action ids on a card.
const (
	actionApprove      = "needletail_approve"
	actionDeny         = "needletail_deny"
	actionApproveTrust = "needletail_approve_trust"
)

// cardChunk is the most characters of escaped payload one section block carries (Slack's limit
// is 3000 per text object, with room for the code fence), maxCardBlocks how many a message may
// hold (Slack's 50).
const (
	cardChunk     = 2800
	maxCardBlocks = 50
)

var _ approvals.Notifier = (*Gateway)(nil)

// SetDecider gives the gateway the approvals service that takes the cards' buttons.
func (g *Gateway) SetDecider(d Decider) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.decider = d
}

// Ask posts a request's card to approver's DM.
func (g *Gateway) Ask(ctx context.Context, r *approvals.Request, approver string) (approvals.Posted, error) {
	select {
	case <-g.ready:
	case <-ctx.Done():
		return approvals.Posted{}, ctx.Err()
	}
	blocks, err := g.cardBlocks(r, true)
	if err != nil {
		return approvals.Posted{}, err
	}
	channel, err := g.o.Web.OpenDM(ctx, approver)
	if err != nil {
		return approvals.Posted{}, err
	}
	ts, err := g.o.Web.PostBlocks(ctx, channel, "", g.cardText(r), blocks)
	if err != nil {
		return approvals.Posted{}, err
	}
	return approvals.Posted{Channel: channel, TS: ts}, nil
}

// Update rewrites a posted card with its request's outcome: the buttons go.
func (g *Gateway) Update(ctx context.Context, r *approvals.Request, p approvals.Posted) error {
	blocks, err := g.cardBlocks(r, false)
	if err != nil {
		return err
	}
	return g.o.Web.UpdateBlocks(ctx, p.Channel, p.TS, outcomeLine(r, g.mention), blocks)
}

// Tell posts a request's outcome in the conversation it was asked in: the runtime's notice (a
// failed before_send handler does not keep it back), never the payload.
func (g *Gateway) Tell(ctx context.Context, r *approvals.Request) error {
	if r.Origin.Channel == "" {
		return nil
	}
	_, err := g.say(ctx, messaging.KindNotice, r.Conversation, r.Origin.Channel, r.Origin.Thread, outcomeLine(r, func(id string) string { return id }))
	return err
}

// mention is a user as a card shows them (<@U…>, which Slack renders as their name).
func (g *Gateway) mention(id string) string { return "<@" + id + ">" }

// outcomeLine is a request's state in one line.
func outcomeLine(r *approvals.Request, who func(string) string) string {
	at := r.DecidedAt.UTC().Format("2006-01-02 15:04 UTC")
	switch r.Status {
	case approvals.StatusPending:
		return fmt.Sprintf("Approval %s is waiting for a decision: %s.", r.ID, r.Card.Title)
	case approvals.StatusApproved:
		return fmt.Sprintf("Approval %s: approved by %s at %s; running %s.", r.ID, who(r.DecidedBy), at, r.Card.Title)
	case approvals.StatusDone:
		trust := ""
		if r.Trusted {
			trust = " The conversation is trusted again."
		}
		return fmt.Sprintf("Approval %s: approved by %s at %s; done: %s.%s", r.ID, who(r.DecidedBy), at, r.Card.Title, trust)
	case approvals.StatusFailed:
		if r.DecidedBy == "" {
			return fmt.Sprintf("Approval %s: %s could not be put to an approver.", r.ID, r.Card.Title)
		}
		return fmt.Sprintf("Approval %s: approved by %s at %s, but running it failed: %s.", r.ID, who(r.DecidedBy), at, r.Card.Title)
	case approvals.StatusDenied:
		return fmt.Sprintf("Approval %s: denied by %s at %s; not done: %s.", r.ID, who(r.DecidedBy), at, r.Card.Title)
	case approvals.StatusExpired:
		return fmt.Sprintf("Approval %s: no decision by %s, so it was denied; not done: %s.", r.ID, r.Expires.UTC().Format("2006-01-02 15:04 UTC"), r.Card.Title)
	}
	return fmt.Sprintf("Approval %s: %s.", r.ID, r.Status)
}

// cardText is a card's notification and fallback text: what is asked and the default.
func (g *Gateway) cardText(r *approvals.Request) string {
	return fmt.Sprintf("Approval needed: %s. If no one decides by %s it is denied (the buttons need the Slack app's Interactivity turned on).",
		r.Card.Title, r.Expires.UTC().Format("2006-01-02 15:04 UTC"))
}

// cardBlocks renders a request as Block Kit, from the structured action only: what, where, why,
// who brought the untrusted input in (with a link to where), the agent's reason labelled as its
// claim, the exact payload, and (open) the buttons, else the outcome.
func (g *Gateway) cardBlocks(r *approvals.Request, open bool) (json.RawMessage, error) {
	type text struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	md := func(s string) text { return text{Type: "mrkdwn", Text: s} }
	section := func(s string) map[string]any { return map[string]any{"type": "section", "text": md(s)} }
	blocks := []map[string]any{
		{"type": "header", "text": text{Type: "plain_text", Text: "Approval needed"}},
		section(fmt.Sprintf("*%s* wants to *%s*", esc(g.agentName()), esc(r.Card.Title))),
	}
	fields := []text{}
	if r.Card.Target != "" {
		fields = append(fields, md("*Target*\n"+esc(r.Card.Target)))
	}
	if r.Action.Tool != "" {
		fields = append(fields, md("*Action*\n`"+esc(r.Action.Tool)+"`"))
	} else {
		fields = append(fields, md("*Action*\n`"+esc(r.Action.Kind)+"`"))
	}
	if r.Conversation != "" {
		where := "`" + esc(r.Conversation) + "`"
		if channel, thread, ok := Origin(r.Conversation); ok {
			where = g.link(channel, thread, "the conversation")
		}
		fields = append(fields, md("*Asked in*\n"+where))
	}
	fields = append(fields, md("*Expires*\n"+r.Expires.UTC().Format("2006-01-02 15:04 UTC")+" (denied without a decision)"))
	blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	blocks = append(blocks, section("*Why a person must approve*\n"+esc(clipRunes(r.Why, 1200))))
	if len(r.Taint) > 0 {
		lines := []string{}
		for i, t := range r.Taint {
			if i == 3 {
				lines = append(lines, fmt.Sprintf("…and %d more", len(r.Taint)-3))
				break
			}
			who := "marked data"
			if len(t.Users) > 0 {
				names := []string{}
				for _, u := range t.Users {
					names = append(names, g.mention(u))
				}
				who = strings.Join(names, ", ")
				if t.Verified {
					who += " (in allowed_users, relaying others' words)"
				} else {
					who += " (not verified: not listed in allowed_users, nor a full member of this workspace)"
				}
			}
			line := "• " + who + ": " + esc(clipRunes(t.Why, 300))
			if t.At != "" {
				line += " — " + esc(t.At)
			}
			if t.Channel != "" {
				line += " — " + g.link(t.Channel, t.Thread, "origin")
			}
			lines = append(lines, line)
		}
		blocks = append(blocks, section("*Untrusted input in the conversation*\n"+strings.Join(lines, "\n")))
	}
	reason := strings.TrimSpace(r.Reason)
	if reason == "" {
		reason = "(the agent gave none)"
	}
	blocks = append(blocks, section("*The agent's stated reason* (its own claim, not verified):\n>"+strings.ReplaceAll(esc(clipRunes(reason, 1500)), "\n", "\n>")))
	if open {
		head := fmt.Sprintf("*Exactly what runs* (%s, sha256 `%s…`):", esc(orDefault(r.Card.PayloadKind, "text")), r.Hash[:16])
		blocks = append(blocks, section(head))
		chunks := chunkEscaped(r.Card.Payload, cardChunk)
		if len(chunks) == 0 {
			chunks = []string{"(empty)"}
		}
		if len(blocks)+len(chunks)+2 > maxCardBlocks {
			return nil, fmt.Errorf("%w: %d blocks", approvals.ErrTooLarge, len(blocks)+len(chunks)+2)
		}
		for _, c := range chunks {
			blocks = append(blocks, section("```"+c+"```"))
		}
		value := r.ID + "|" + r.Hash
		buttons := []map[string]any{
			{"type": "button", "action_id": actionApprove, "style": "primary", "value": value, "text": text{Type: "plain_text", Text: "Approve"}},
			{"type": "button", "action_id": actionDeny, "style": "danger", "value": value, "text": text{Type: "plain_text", Text: "Deny"}},
		}
		if r.Trust {
			buttons = append(buttons, map[string]any{"type": "button", "action_id": actionApproveTrust, "value": value, "text": text{Type: "plain_text", Text: "Approve & trust thread"}})
		}
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": "needletail_approval", "elements": buttons})
		blocks = append(blocks, map[string]any{"type": "context", "elements": []text{md(fmt.Sprintf("Approval %s. Only this exact payload runs, once. If the buttons do nothing, the Slack app's Interactivity is off: the request is denied at %s.",
			r.ID, r.Expires.UTC().Format("15:04 UTC")))}})
	} else {
		blocks = append(blocks, section(outcomeLine(r, g.mention)))
		blocks = append(blocks, map[string]any{"type": "context", "elements": []text{md(fmt.Sprintf("Approval %s · sha256 `%s…`", r.ID, r.Hash[:16]))}})
	}
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false) // mrkdwn's own escapes (&amp; &lt; &gt;) are already in the text
	if err := enc.Encode(blocks); err != nil {
		return nil, err
	}
	return json.RawMessage(strings.TrimSpace(b.String())), nil
}

func (g *Gateway) agentName() string {
	if id := g.identity(); id.User != "" {
		return id.User
	}
	return "The agent"
}

// identity is auth.test's answer once the gateway has it (zero before: it is written once, before
// ready is closed).
func (g *Gateway) identity() Identity {
	select {
	case <-g.ready:
		return g.id
	default:
		return Identity{}
	}
}

// link is a Slack link to a message (a thread's, or a DM's), or the ids as text when the
// workspace's address is unknown.
func (g *Gateway) link(channel, thread, label string) string {
	if p := g.Permalink(channel, thread); p != "" {
		return "<" + p + "|" + label + ">"
	}
	if thread != "" {
		return "`" + channel + "` thread `" + thread + "`"
	}
	return "`" + channel + "`"
}

// Permalink is Slack's link to a message (ts) in channel, or to the channel when ts is "": built
// from the workspace's address (auth.test's url), no API call; "" when it is unknown.
func (g *Gateway) Permalink(channel, ts string) string {
	base := strings.TrimRight(g.identity().URL, "/")
	if base == "" || !strings.HasPrefix(base, "https://") || !channelIDRe.MatchString(channel) {
		return ""
	}
	if ts == "" {
		return base + "/archives/" + channel
	}
	return base + "/archives/" + channel + "/p" + strings.Replace(ts, ".", "", 1)
}

// esc escapes text for mrkdwn: &, < and > (Slack's control characters); everything else as is.
func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// chunkEscaped splits s into pieces whose escaped form is at most n characters, never inside an
// escape; a run of three backticks is broken with a zero-width space so it cannot end the fence.
func chunkEscaped(s string, n int) []string {
	s = strings.ReplaceAll(s, "```", "`​``")
	var out []string
	var b strings.Builder
	for _, r := range s {
		e := esc(string(r))
		if b.Len()+len(e) > n {
			out = append(out, b.String())
			b.Reset()
		}
		b.WriteString(e)
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// interaction is a block_actions payload (the fields used here).
type interaction struct {
	Type string `json:"type"`
	User struct {
		ID string `json:"id"`
	} `json:"user"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
	Container struct {
		ChannelID string `json:"channel_id"`
		MessageTS string `json:"message_ts"`
	} `json:"container"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
}

// interact takes an interactive payload from the socket (acked already) and decides off the
// reader. Only a button on one of a request's own cards counts; anything else is ignored.
func (g *Gateway) interact(payload json.RawMessage) {
	var in interaction
	if json.Unmarshal(payload, &in) != nil || in.Type != "block_actions" || len(in.Actions) == 0 || in.User.ID == "" {
		return
	}
	g.mu.Lock()
	d := g.decider
	g.mu.Unlock()
	if d == nil {
		g.logf("slack: an approval button was clicked but approvals are off")
		return
	}
	a := in.Actions[0]
	if a.ActionID != actionApprove && a.ActionID != actionDeny && a.ActionID != actionApproveTrust {
		return
	}
	id, hash, ok := strings.Cut(a.Value, "|")
	if !ok {
		return
	}
	g.notes.Add(1)
	go func() {
		defer g.notes.Done()
		channel := orDefault(in.Container.ChannelID, in.Channel.ID)
		r, found := d.Get(id)
		if !found || !slices.ContainsFunc(r.Cards, func(p approvals.Posted) bool { return p.Channel == channel && p.TS == in.Container.MessageTS }) {
			g.logf("slack: a click on %s from %s does not come from its card: ignored", id, in.User.ID)
			return
		}
		ctx, cancel := context.WithTimeout(g.base, 10*time.Minute)
		defer cancel()
		_, err := d.Decide(ctx, approvals.Decision{ID: id, Hash: hash, By: in.User.ID, Approve: a.ActionID != actionDeny, Trust: a.ActionID == actionApproveTrust})
		if err == nil {
			return
		}
		g.logf("slack: %s: the decision of %s was not taken: %v", id, in.User.ID, err)
		why := err.Error()
		if errors.Is(err, approvals.ErrDecided) {
			why = "this request was already decided"
		}
		ectx, ecancel := context.WithTimeout(g.base, 10*time.Second)
		defer ecancel()
		if eerr := g.o.Web.Ephemeral(ectx, channel, in.User.ID, "Not taken: "+why+"."); eerr != nil {
			g.logf("slack: %s: could not tell %s why: %v", id, in.User.ID, eerr)
		}
	}()
}
