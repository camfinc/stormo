package whatsapp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// An accepted message is recorded before it is claimed, and the turn carries its origin; a
// reaction is not recorded.
func TestOriginRecordedBeforeTheClaim(t *testing.T) {
	c := open(t, Policy{Enabled: true, Users: []string{"393331234567"}, Groups: []string{group.String()}}, nil)
	c.names[group] = "Spring promo"
	var got sends
	got.record(c)
	var events []OriginEvent
	c.UseOrigins(func(ctx context.Context, e OriginEvent) (string, error) {
		var claimed int
		_ = c.db.QueryRow(`SELECT count(*) FROM stormo_received WHERE id = ?`, e.Chat+":"+e.MessageID).Scan(&claimed)
		if claimed != 0 {
			t.Error("claimed before it was recorded")
		}
		events = append(events, e)
		return "orig-1", nil
	})
	var turns []Turn
	c.UseExchange(func(_ context.Context, session string, tu Turn) (Answer, error) {
		turns = append(turns, tu)
		return Answer{Text: "ok"}, nil
	})
	c.handle(context.Background(), inGroup("@393330000000 sizes?", &waE2E.ContextInfo{MentionedJID: []string{me.PN.String()}}))
	if len(events) != 1 || len(turns) != 1 || turns[0].Origin != "orig-1" {
		t.Fatalf("events %+v turns %+v", events, turns)
	}
	e := events[0]
	if !e.Group || e.Chat != group.String() || e.MessageID != "in-2" || e.Participant != member.String() || len(e.Quote) == 0 || !strings.HasPrefix(e.Session, "whatsapp-") ||
		e.Account != c.account() {
		t.Errorf("recorded %+v", e)
	}
	c.handle(context.Background(), reaction(inGroup("", nil), "sent-120363012345678901", "👍"))
	if len(events) != 1 {
		t.Errorf("a reaction was recorded as an origin: %+v", events)
	}
}

// A turn that runs out of time still tells the person it could not complete: the reply and its
// reaction go on a context of their own, not the expired turn's.
func TestTimedOutTurnStillReplies(t *testing.T) {
	c := open(t, Policy{Enabled: true, Users: []string{"393331234567"}}, nil)
	c.turnTimeout = 30 * time.Millisecond
	var got []string
	c.send = func(ctx context.Context, _ types.JID, m *waE2E.Message) (types.MessageID, error) {
		if ctx.Err() != nil {
			return "", ctx.Err() // what the real client does with an expired context
		}
		if r := m.GetReactionMessage(); r != nil {
			got = append(got, "react:"+r.GetText())
		} else {
			got = append(got, m.GetConversation()+m.GetExtendedTextMessage().GetText())
		}
		return "sent-1", nil
	}
	c.UseExchange(func(ctx context.Context, _ string, _ Turn) (Answer, error) {
		<-ctx.Done()
		return Answer{}, ctx.Err()
	})
	c.handle(context.Background(), direct("hola"))
	if len(got) < 2 || !strings.Contains(got[len(got)-2], "could not complete") || got[len(got)-1] != "react:"+failed {
		t.Fatalf("after a timeout: %q", got)
	}
}

// PostOrigin posts one message, quoting the person's message in a group, and tells the journal
// before it goes and after, with its id.
func TestPostOrigin(t *testing.T) {
	c := open(t, Policy{Enabled: true, Groups: []string{group.String()}}, nil)
	c.connected = func() bool { return true }
	var got sends
	got.record(c)
	plain := c.send
	var quotes []string
	c.send = func(ctx context.Context, chat types.JID, m *waE2E.Message) (types.MessageID, error) {
		quotes = append(quotes, m.GetExtendedTextMessage().GetContextInfo().GetStanzaID())
		return plain(ctx, chat, m)
	}
	asked, _ := proto.Marshal(&waE2E.Message{Conversation: proto.String("sizes?")})
	o := Origin{Chat: group.String(), Group: true, MessageID: "in-2", Participant: member.String(), Quote: asked, Account: c.account()}
	var journal []string
	note := func(state, id string) error {
		journal = append(journal, state+":"+id)
		return nil
	}
	if err := c.PostOrigin(context.Background(), o, "the sizes", note); err != nil {
		t.Fatal(err)
	}
	if strings.Join(journal, " ") != "sending: sent:sent-120363012345678901" || len(quotes) != 1 || quotes[0] != "in-2" || got[0] != "the sizes" {
		t.Errorf("journal %v quotes %v posts %v", journal, quotes, got)
	}
	got = nil
	stop := errors.New("journal down")
	err := c.PostOrigin(context.Background(), o, "again", func(state, _ string) error {
		if state == "sending" {
			return stop
		}
		return nil
	})
	var pe *PostError
	if !errors.Is(err, stop) || len(got) != 0 || !errors.As(err, &pe) || !pe.Unsent {
		t.Errorf("a journal refusal: %v, posted %v", err, got)
	}
}

// What a stopped post says: provably unsent (refused before it went, or by the server), sent but
// not journaled (with its id), or unknown (it may have gone).
func TestPostOriginErrorClasses(t *testing.T) {
	p := Policy{Enabled: true, Groups: []string{group.String()}}
	c := open(t, p, nil)
	c.policy = func() Policy { return p } // changed by the test below
	c.connected = func() bool { return true }
	o := Origin{Chat: group.String(), Group: true, MessageID: "in-2", Participant: member.String(), Account: c.account()}
	ok := func(string, string) error { return nil }
	for _, tc := range []struct {
		err    error
		unsent bool
	}{
		{whatsmeow.ErrNotConnected, true},
		{fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError), true},
		{whatsmeow.ErrMessageTimedOut, false},
		{context.DeadlineExceeded, false},
		{errors.New("failed to encrypt"), false},
	} {
		c.send = func(context.Context, types.JID, *waE2E.Message) (types.MessageID, error) { return "", tc.err }
		var pe *PostError
		if err := c.PostOrigin(context.Background(), o, "a", ok); !errors.As(err, &pe) || pe.Unsent != tc.unsent || pe.Sent {
			t.Errorf("%v: %+v", tc.err, pe)
		}
	}
	var sent []string
	c.send = func(_ context.Context, _ types.JID, m *waE2E.Message) (types.MessageID, error) {
		sent = append(sent, m.GetConversation()+m.GetExtendedTextMessage().GetText())
		return "wa-9", nil
	}
	var pe *PostError
	err := c.PostOrigin(context.Background(), o, "a", func(state, _ string) error {
		if state == "sent" {
			return errors.New("disk full")
		}
		return nil
	})
	if !errors.As(err, &pe) || !pe.Sent || pe.Unsent || pe.ID != "wa-9" {
		t.Errorf("sent but not journaled: %+v", pe)
	}
	// the group leaves the allowed list, or another device is linked: nothing goes
	sent = nil
	p.Groups = nil
	if err := c.PostOrigin(context.Background(), o, "a", ok); !errors.As(err, &pe) || !pe.Unsent || len(sent) != 0 {
		t.Errorf("revoked: %v, sent %v", err, sent)
	}
	p.Groups = []string{group.String()}
	if err := c.PostOrigin(context.Background(), o, "a", ok); err != nil || len(sent) != 1 {
		t.Fatalf("allowed again: %v, sent %v", err, sent)
	}
	// unlinked and paired again (the same number, whatever device address it gets): another link
	c.relink()
	sent = nil
	if err := c.PostOrigin(context.Background(), o, "a", ok); !errors.As(err, &pe) || !pe.Unsent || len(sent) != 0 {
		t.Errorf("after a relink: %v, sent %v", err, sent)
	}
}

// The link id is the session's: the same across a restart, another after a relink.
func TestLinkSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.db")
	none := func() Policy { return Policy{} }
	a, err := Open(path, none, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := a.account()
	a.Close()
	b, err := Open(path, none, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if id == "" || b.account() != id {
		t.Fatalf("restart: %q then %q", id, b.account())
	}
	b.relink()
	if b.account() == id {
		t.Fatal("a relink kept the link id")
	}
}

// A person no longer admitted while their turn ran gets nothing, not even the failure reply.
func TestRevokedWhileTheTurnRan(t *testing.T) {
	p := Policy{Enabled: true, Users: []string{"393331234567"}}
	c := open(t, p, nil)
	c.policy = func() Policy { return p }
	var got sends
	got.record(c)
	c.UseExchange(func(context.Context, string, Turn) (Answer, error) {
		p.Users = nil // removed from allowed_users meanwhile
		return Answer{Text: "here you go"}, nil
	})
	c.handle(context.Background(), direct("hola"))
	for _, s := range got {
		if !strings.HasPrefix(s, "react:") || s != "react:"+working {
			t.Fatalf("posted to someone no longer admitted: %q", got)
		}
	}
}

func TestAllowedReply(t *testing.T) {
	p := Policy{Enabled: true, Users: []string{"+393331234567"}, Groups: []string{group.String()}}
	c := open(t, p, nil)
	c.connected = func() bool { return true }
	for _, ok := range []Origin{{Chat: group.String(), Group: true, Account: c.account()}, {Chat: owner.String(), Number: "393331234567", Account: c.account()}} {
		if err := c.AllowedReply(ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []Origin{{Chat: "1203630999@g.us", Group: true, Account: c.account()}, {Chat: member.String(), Number: "393339999999", Account: c.account()},
		{Chat: group.String(), Group: true}, {Chat: group.String(), Group: true, Account: "another-link"}} {
		if err := c.AllowedReply(bad); err == nil {
			t.Errorf("%+v allowed", bad)
		}
	}
	c.connected = func() bool { return false }
	if err := c.AllowedReply(Origin{Chat: group.String(), Group: true, Account: c.account()}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("not connected: %v", err)
	}
}
