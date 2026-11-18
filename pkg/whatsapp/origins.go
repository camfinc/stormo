package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Origins (docs/core.md §5, "Returns"): a person's message the core records, before the connector
// claims it, so the outcome of a request the agent sends while answering it can come back to that
// person, in that chat, quoting what they asked.

// OriginEvent is an accepted message as the core records it.
type OriginEvent struct {
	Session, Chat, MessageID, Participant, Number, Name string
	Group, Verified                                     bool
	// Quote is the message as received (protobuf), to quote it later.
	Quote []byte
}

// Origin is a recorded origin as the connector posts to it.
type Origin struct {
	Chat, MessageID, Participant, Number string
	Group                                bool
	Quote                                []byte
}

// ErrPending is an Exchange's answer for a turn still running when the connector stopped waiting:
// nothing is posted now; its reply comes later, from the core.
var ErrPending = errors.New("whatsapp: the turn is still running, its reply comes later")

// UseOrigins has the connector record each accepted message (a reaction is not one) through
// register before it claims it; the origin id register answers goes with the turn ("" none).
func (c *Connector) UseOrigins(register func(ctx context.Context, e OriginEvent) (string, error)) {
	c.mu.Lock()
	c.register = register
	c.mu.Unlock()
}

// maxPart is the most runes one message part holds.
const maxPart = 3500

// Parts is text as the connector posts it: parts of at most 3500 runes, always split the same way.
func Parts(text string) []string {
	var out []string
	r := []rune(text)
	for len(r) > 0 {
		n := min(len(r), maxPart)
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	return out
}

// AllowedReply is whether the connector may post to o now: the account linked and connected, its
// group still allowed, or its person still admitted (allowed_users, or anyone when the agent takes
// messages from anyone). It is a reply to that person, not an unprompted post.
func (c *Connector) AllowedReply(o Origin) error {
	if !c.connected() {
		return ErrNotConnected
	}
	p := c.policy()
	switch {
	case !p.Enabled:
		return errors.New("whatsapp: the channel is off")
	case o.Group:
		jid, err := types.ParseJID(o.Chat)
		if err != nil || !slices.Contains(p.Groups, jid.ToNonAD().String()) {
			return fmt.Errorf("whatsapp: %s is no longer one of the allowed_groups", o.Chat)
		}
	case !p.All && !slices.ContainsFunc(p.Users, func(u string) bool { return strings.TrimPrefix(u, "+") == o.Number && o.Number != "" }):
		return errors.New("whatsapp: the person is no longer one of the allowed_users")
	}
	return nil
}

// PostOrigin posts parts[from:] to o's chat through the posting lane, the first part quoting the
// person's message in a group when from is 0. journal is told each part before it goes ("sending")
// and after ("sent", with its id); its error stops the post before that part. A send error stops it
// too: the parts after it are not sent.
func (c *Connector) PostOrigin(ctx context.Context, o Origin, parts []string, from int, journal func(part int, state, id string) error) error {
	chat, err := types.ParseJID(o.Chat)
	if err != nil {
		return fmt.Errorf("whatsapp: chat %q: %w", o.Chat, err)
	}
	var quote *waE2E.ContextInfo
	if o.Group && from == 0 && o.MessageID != "" {
		quote = &waE2E.ContextInfo{StanzaID: proto.String(o.MessageID), Participant: proto.String(o.Participant)}
		if len(o.Quote) > 0 {
			var m waE2E.Message
			if proto.Unmarshal(o.Quote, &m) == nil {
				quote.QuotedMessage = &m
			}
		}
	}
	c.lane.Lock()
	defer c.lane.Unlock()
	for i := from; i < len(parts); i++ {
		if err := journal(i, "sending", ""); err != nil {
			return err
		}
		m := &waE2E.Message{Conversation: proto.String(parts[i])}
		if quote != nil {
			m, quote = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(parts[i]), ContextInfo: quote}}, nil
		}
		id, err := c.send(ctx, chat, m)
		if err != nil {
			return err
		}
		_, _ = c.db.ExecContext(ctx, `INSERT OR REPLACE INTO stormo_sent(id,chat,excerpt,at) VALUES (?,?,?,?)`, id, chat.ToNonAD().String(), clean(parts[i], 200), time.Now().Unix())
		c.log(ctx, logKey(chat), Line{ID: string(id), At: time.Now(), Mine: true, Text: parts[i]})
		if err := journal(i, "sent", string(id)); err != nil {
			return err
		}
	}
	return nil
}
