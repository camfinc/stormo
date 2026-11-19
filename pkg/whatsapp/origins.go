package whatsapp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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

// OriginEvent is an accepted message as the core records it. Account is the link that received it:
// an origin is answered only from that link (pairing or unlinking makes another).
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

// account is the link's identity: a random id the session keeps, made anew whenever the account is
// paired or unlinked (a relink, even of the same number on a device of the same address, is another
// link), so an origin is answered only from the link that received it. "" while a new one could not
// be kept: nothing is recorded or posted under a link that is not on disk (it is tried again here).
func (c *Connector) account() string {
	c.mu.Lock()
	pending, link := c.relinking, c.link
	c.mu.Unlock()
	if pending && c.relink() == nil {
		c.mu.Lock()
		link = c.link
		c.mu.Unlock()
	}
	return link
}

// device is the linked device's own address ("" unlinked): kept beside the link id, so a session
// found on another device than its id was made for is relinked even if that rotation was lost.
func (c *Connector) device() string {
	if c.me == nil {
		return ""
	}
	return c.me().PN.String()
}

// relink makes the session a new link id: every origin recorded before is refused from now on. The
// old id is dropped first and the new one used only once it is on disk; until then there is none.
func (c *Connector) relink() error {
	c.mu.Lock()
	c.link, c.relinking = "", true
	c.mu.Unlock()
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	if _, err := c.db.Exec(`INSERT OR REPLACE INTO stormo_meta(k, v) VALUES ('link', ?)`, id+"|"+c.device()); err != nil {
		return err
	}
	c.mu.Lock()
	c.link, c.relinking = id, false
	c.mu.Unlock()
	return nil
}

// loadLink reads the session's link id at open: a new one when there is none, or when the device it
// was made for is no longer the linked one.
func (c *Connector) loadLink() error {
	var v string
	err := c.db.QueryRow(`SELECT v FROM stormo_meta WHERE k = 'link'`).Scan(&v)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return c.relink()
	case err != nil:
		return err
	}
	id, dev, _ := strings.Cut(v, "|")
	if id == "" || dev != c.device() {
		return c.relink()
	}
	c.mu.Lock()
	c.link = id
	c.mu.Unlock()
	return nil
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
		return errors.New("whatsapp: the account was relinked since the person wrote")
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
