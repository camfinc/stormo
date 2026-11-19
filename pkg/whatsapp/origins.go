package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Origins (docs/core.md §5, "Returns"): a person's message the core records, before the connector
// claims it, so the outcome of a request the agent sends while answering it can come back to that
// person, in that chat, quoting what they asked.

// OriginEvent is an accepted message as the core records it. Account is the linked device that
// received it: an origin is answered only from that device (unlinking or relinking makes another).
type OriginEvent struct {
	Session, Chat, MessageID, Participant, Number, Name, Account string
	Group, Verified                                              bool
	// Quote is the message as received (protobuf), to quote it later.
	Quote []byte
}

// Origin is a recorded origin as the connector posts to it.
type Origin struct {
	Chat, MessageID, Participant, Number, Account string
	Group                                         bool
	Quote                                         []byte
}

// PostError is why PostOrigin stopped. Unsent: the message provably did not go (the account was
// not connected, the server refused it, the post was no longer allowed or the journal refused it):
// it may go again. Sent: it went (ID) but the journal could not record it. Neither: it may or may
// not have gone, and must never be sent again on its own.
type PostError struct {
	Unsent, Sent bool
	ID           string
	Err          error
}

func (e *PostError) Error() string { return "whatsapp: " + e.Err.Error() }
func (e *PostError) Unwrap() error { return e.Err }

// unsent is whether a send error proves the message did not go: whatsmeow answered it before
// writing the message (no connection, no device, no permission, an address it cannot send to) or
// the server refused it. A timeout, a cancelled wait or anything else may have gone.
func unsent(err error) bool {
	for _, e := range []error{ErrNotConnected, whatsmeow.ErrClientIsNil, whatsmeow.ErrNotConnected, whatsmeow.ErrNotLoggedIn,
		whatsmeow.ErrUnknownServer, whatsmeow.ErrRecipientADJID, whatsmeow.ErrBroadcastListUnsupported,
		whatsmeow.ErrServerReturnedError, whatsmeow.ErrNoPermissionInGroup} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}

// account is the linked device's own address, device included ("" when not linked): a relink,
// even of the same number, is another device.
func (c *Connector) account() string {
	if c.me == nil {
		return ""
	}
	if pn := c.me().PN; !pn.IsEmpty() {
		return pn.String()
	}
	return ""
}

// UseOrigins has the connector record each accepted message (a reaction is not one) through
// register before it claims it; the origin id register answers goes with the turn ("" none).
func (c *Connector) UseOrigins(register func(ctx context.Context, e OriginEvent) (string, error)) {
	c.mu.Lock()
	c.register = register
	c.mu.Unlock()
}

// AllowedReply is whether the connector may post to o now: the device that received it linked and
// connected, its group still allowed, or its person still admitted (allowed_users, or anyone when
// the agent takes messages from anyone). It is a reply to that person, not an unprompted post.
func (c *Connector) AllowedReply(o Origin) error {
	if !c.connected() {
		return ErrNotConnected
	}
	if acct := c.account(); o.Account == "" || acct != o.Account {
		return errors.New("whatsapp: the linked device is not the one the person wrote to")
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

// PostOrigin posts text to o's chat as one message, through the posting lane, quoting the person's
// message in a group. Under the lane the post is checked to be still allowed (AllowedReply) and the
// journal is told before it goes ("sending"; its error stops it, unsent) and after ("sent", with
// its id). Any stop is a *PostError.
func (c *Connector) PostOrigin(ctx context.Context, o Origin, text string, journal func(state, id string) error) error {
	chat, err := types.ParseJID(o.Chat)
	if err != nil {
		return &PostError{Unsent: true, Err: fmt.Errorf("chat %q: %w", o.Chat, err)}
	}
	m := &waE2E.Message{Conversation: proto.String(text)}
	if o.Group && o.MessageID != "" {
		quote := &waE2E.ContextInfo{StanzaID: proto.String(o.MessageID), Participant: proto.String(o.Participant)}
		if len(o.Quote) > 0 {
			var q waE2E.Message
			if proto.Unmarshal(o.Quote, &q) == nil {
				quote.QuotedMessage = &q
			}
		}
		m = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: quote}}
	}
	c.lane.Lock()
	defer c.lane.Unlock()
	if err := c.AllowedReply(o); err != nil {
		return &PostError{Unsent: true, Err: err}
	}
	if err := journal("sending", ""); err != nil {
		return &PostError{Unsent: true, Err: err}
	}
	id, err := c.send(ctx, chat, m)
	if err != nil {
		return &PostError{Unsent: unsent(err), Err: err}
	}
	_, _ = c.db.ExecContext(context.WithoutCancel(ctx), `INSERT OR REPLACE INTO stormo_sent(id,chat,excerpt,at) VALUES (?,?,?,?)`, id, chat.ToNonAD().String(), clean(text, 200), time.Now().Unix())
	c.log(context.WithoutCancel(ctx), logKey(chat), Line{ID: string(id), At: time.Now(), Mine: true, Text: text})
	if err := journal("sent", string(id)); err != nil {
		return &PostError{Sent: true, ID: string(id), Err: err}
	}
	return nil
}
