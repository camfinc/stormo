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

// cardChunk is the most characters of payload one rich text block carries (Slack's limit is
// 3000 per text object), maxCardBlocks how many a message may hold (Slack's 50).
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
	return g.o.Web.UpdateBlocks(ctx, p.Channel, p.TS, outcomeLine(r, g.mention, inlineCode), blocks)
}

// Tell posts a request's outcome in the conversation it was asked in: the runtime's notice (a
// failed before_send handler does not keep it back), never the payload. The notice is Markdown
// (say escapes it): the title goes in as code, never as formatting.
func (g *Gateway) Tell(ctx context.Context, r *approvals.Request) error {
	if r.Origin.Channel == "" {
		return nil
	}
	_, err := g.say(ctx, messaging.KindNotice, r.Conversation, r.Origin.Channel, r.Origin.Thread, outcomeLine(r, func(id string) string { return id }, markdownCode))
	return err
}

// mention is a user as a card shows them (<@U…>, which Slack renders as their name).
func (g *Gateway) mention(id string) string { return "<@" + id + ">" }

// Limits of what a card shows of the model's words (Slack's own: 3000 characters a section's
// text, 2000 a field's, 150 a header's; the message 50 blocks).
const (
	titleMax  = 300
	targetMax = 600
	reasonMax = 1500
	whyMax    = 1200
)

// inlineCode is model-controlled text as a card shows it in mrkdwn: one line, clipped, its
// backticks turned into a lookalike (so it cannot close the code span) and &, < and > escaped
// (no mention, link or broadcast), inside inline code (no formatting).
func inlineCode(s string) string { return "`" + esc(codeText(s)) + "`" }

// markdownCode is the same for Markdown the runtime posts through say (which escapes it).
func markdownCode(s string) string { return "`" + codeText(s) + "`" }

func codeText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(clipRunes(s, titleMax), "`", "ˋ")
}

// outcomeLine is a request's state in one line; code renders the title (the model's words).
func outcomeLine(r *approvals.Request, who, code func(string) string) string {
	at := r.DecidedAt.UTC().Format("2006-01-02 15:04 UTC")
	title := code(r.Card.Title)
	switch r.Status {
	case approvals.StatusPending:
		return fmt.Sprintf("Approval %s is waiting for a decision: %s.", r.ID, title)
	case approvals.StatusApproved:
		return fmt.Sprintf("Approval %s: approved by %s at %s; running %s.", r.ID, who(r.DecidedBy), at, title)
	case approvals.StatusDone:
		trust := ""
		if r.Trusted {
			trust = " The conversation is trusted again."
		}
		return fmt.Sprintf("Approval %s: approved by %s at %s; done: %s.%s", r.ID, who(r.DecidedBy), at, title, trust)
	case approvals.StatusFailed:
		if r.DecidedBy == "" {
			return fmt.Sprintf("Approval %s: %s could not be put to an approver.", r.ID, title)
		}
		return fmt.Sprintf("Approval %s: approved by %s at %s, but running it failed: %s.", r.ID, who(r.DecidedBy), at, title)
	case approvals.StatusDenied:
		return fmt.Sprintf("Approval %s: denied by %s at %s; not done: %s.", r.ID, who(r.DecidedBy), at, title)
	case approvals.StatusExpired:
		return fmt.Sprintf("Approval %s: no decision by %s, so it was denied; not done: %s.", r.ID, r.Expires.UTC().Format("2006-01-02 15:04 UTC"), title)
	case approvals.StatusInterrupted:
		return fmt.Sprintf("Approval %s: approved by %s at %s, but the runtime stopped before it finished; it is not run again: %s.", r.ID, who(r.DecidedBy), at, title)
	}
	return fmt.Sprintf("Approval %s: %s.", r.ID, r.Status)
}

// cardText is a card's notification and fallback text: what is asked and the default.
func (g *Gateway) cardText(r *approvals.Request) string {
	return fmt.Sprintf("Approval needed: %s. If no one decides by %s it is denied (the buttons need the Slack app's Interactivity turned on).",
		inlineCode(r.Card.Title), r.Expires.UTC().Format("2006-01-02 15:04 UTC"))
}

// cardBlocks renders a request as Block Kit, from the structured action only: what, where, why,
// who brought the untrusted input in (with a link to where), the agent's reason labelled as its
// claim, the exact payload, and (open) the buttons, else the outcome. The model's words (the
// title's and target's parts, the reason) are shown as code or plain text, clipped, never as
// formatting; the payload as preformatted rich text, exactly (no markup is read in it); every
// block within Slack's limits, so no field the model sizes can keep the card from posting.
func (g *Gateway) cardBlocks(r *approvals.Request, open bool) (json.RawMessage, error) {
	type text struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	md := func(s string) text { return text{Type: "mrkdwn", Text: clipRunes(s, 2900)} }
	section := func(s string) map[string]any { return map[string]any{"type": "section", "text": md(s)} }
	plain := func(s string) map[string]any {
		return map[string]any{"type": "section", "text": text{Type: "plain_text", Text: clipRunes(s, 2900)}}
	}
	blocks := []map[string]any{
		{"type": "header", "text": text{Type: "plain_text", Text: "Approval needed"}},
		section(fmt.Sprintf("*%s* wants to %s", esc(clipRunes(g.agentName(), 80)), inlineCode(r.Card.Title))),
	}
	fields := []text{}
	if r.Card.Target != "" {
		fields = append(fields, md("*Target*\n"+"`"+esc(strings.ReplaceAll(clipRunes(strings.Join(strings.Fields(r.Card.Target), " "), targetMax), "`", "ˋ"))+"`"))
	}
	if r.Action.Tool != "" {
		fields = append(fields, md("*Action*\n"+inlineCode(r.Action.Tool)))
	} else {
		fields = append(fields, md("*Action*\n"+inlineCode(r.Action.Kind)))
	}
	if r.Conversation != "" {
		where := inlineCode(r.Conversation)
		if channel, thread, ok := Origin(r.Conversation); ok {
			where = g.link(channel, thread, "the conversation")
		}
		fields = append(fields, md("*Asked in*\n"+where))
	}
	fields = append(fields, md("*Expires*\n"+r.Expires.UTC().Format("2006-01-02 15:04 UTC")+" (denied without a decision)"))
	blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	blocks = append(blocks, section("*Why a person must approve*\n"+esc(clipRunes(r.Why, whyMax))))
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
				for j, u := range t.Users {
					if j == 5 {
						names = append(names, fmt.Sprintf("%d more", len(t.Users)-5))
						break
					}
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
				line += " — " + esc(clipRunes(t.At, 40))
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
	blocks = append(blocks, section("*The agent's stated reason* (its own claim, not verified):"), plain(clipRunes(reason, reasonMax)))
	if open {
		head := fmt.Sprintf("*Exactly what runs* (%s, sha256 `%s…`):", esc(orDefault(r.Card.PayloadKind, "text")), r.Hash[:16])
		blocks = append(blocks, section(head))
		chunks := chunkRunes(r.Card.Payload, cardChunk)
		if len(chunks) == 0 {
			chunks = []string{"(empty)"}
		}
		room := maxCardBlocks - len(blocks) - 2 // the buttons and the context
		if len(chunks) > room {
			// never more than a message holds: the rest is cut, said so (the sha256 covers the
			// whole payload, and Approve runs the whole of it)
			shown := 0
			for _, c := range chunks[:room-1] {
				shown += len(c)
			}
			chunks = chunks[:room-1]
			blocks = append(blocks, preformatted(chunks)...)
			blocks = append(blocks, section(fmt.Sprintf("*The payload is cut here*: %d more bytes are not shown, and Approve runs them too. Deny unless you have seen it whole.", len(r.Card.Payload)-shown)))
		} else {
			blocks = append(blocks, preformatted(chunks)...)
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
		blocks = append(blocks, section(outcomeLine(r, g.mention, inlineCode)))
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

// preformatted are payload chunks as rich text blocks: preformatted text, shown as it is (no
// mrkdwn is read in it, so no run of backticks can close it).
func preformatted(chunks []string) []map[string]any {
	out := make([]map[string]any, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, map[string]any{"type": "rich_text", "elements": []map[string]any{{"type": "rich_text_preformatted",
			"elements": []map[string]any{{"type": "text", "text": c}}}}})
	}
	return out
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

// chunkRunes splits s into pieces of at most n characters (never inside a character).
func chunkRunes(s string, n int) []string {
	var out []string
	for s != "" {
		cut, count := 0, 0
		for cut < len(s) && count < n {
			_, size := utf8.DecodeRuneInString(s[cut:])
			cut += size
			count++
		}
		out = append(out, s[:cut])
		s = s[cut:]
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
