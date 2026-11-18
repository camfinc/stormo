package whatsapp

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	if !e.Group || e.Chat != group.String() || e.MessageID != "in-2" || e.Participant != member.String() || len(e.Quote) == 0 || !strings.HasPrefix(e.Session, "whatsapp-") {
		t.Errorf("recorded %+v", e)
	}
	c.handle(context.Background(), reaction(inGroup("", nil), "sent-120363012345678901", "👍"))
	if len(events) != 1 {
		t.Errorf("a reaction was recorded as an origin: %+v", events)
	}
}

// A turn still running when the connector stopped waiting posts nothing now.
func TestPendingPostsNothing(t *testing.T) {
	c := open(t, Policy{Enabled: true, Users: []string{"393331234567"}}, nil)
	var got sends
	got.record(c)
	c.UseExchange(func(context.Context, string, Turn) (Answer, error) { return Answer{}, ErrPending })
	c.handle(context.Background(), direct("hola"))
	for _, s := range got {
		if !strings.HasPrefix(s, "react:") {
			t.Fatalf("posted %q for a pending turn", s)
		}
	}
}

// PostOrigin tells the journal each part before and after it goes, quotes the person's message on
// a group's first part, resumes from a later part, and stops at a journal or send error.
func TestPostOrigin(t *testing.T) {
	c := open(t, Policy{Enabled: true, Groups: []string{group.String()}}, nil)
	var got sends
	got.record(c)
	plain := c.send
	var quotes []string
	c.send = func(ctx context.Context, chat types.JID, m *waE2E.Message) (types.MessageID, error) {
		quotes = append(quotes, m.GetExtendedTextMessage().GetContextInfo().GetStanzaID())
		return plain(ctx, chat, m)
	}
	asked, _ := proto.Marshal(&waE2E.Message{Conversation: proto.String("sizes?")})
	o := Origin{Chat: group.String(), Group: true, MessageID: "in-2", Participant: member.String(), Quote: asked}
	var journal []string
	note := func(part int, state, id string) error {
		journal = append(journal, state+":"+string(rune('0'+part))+":"+id)
		return nil
	}
	if err := c.PostOrigin(context.Background(), o, []string{"first", "second"}, 0, note); err != nil {
		t.Fatal(err)
	}
	if strings.Join(journal, " ") != "sending:0: sent:0:sent-120363012345678901 sending:1: sent:1:sent-120363012345678901" {
		t.Errorf("journal %v", journal)
	}
	if len(quotes) != 2 || quotes[0] != "in-2" || quotes[1] != "" || got[0] != "first" || got[1] != "second" {
		t.Errorf("quotes %v posts %v", quotes, got)
	}
	got, quotes = nil, nil
	if err := c.PostOrigin(context.Background(), o, []string{"first", "second"}, 1, note); err != nil || len(got) != 1 || got[0] != "second" || quotes[0] != "" {
		t.Errorf("resumed: %v %v %v", got, quotes, err)
	}
	got = nil
	stop := errors.New("journal down")
	err := c.PostOrigin(context.Background(), o, []string{"first", "second"}, 0, func(part int, state, id string) error {
		if part == 1 && state == "sending" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || len(got) != 1 {
		t.Errorf("a journal error: %v, posted %v", err, got)
	}
}

func TestAllowedReplyAndParts(t *testing.T) {
	p := Policy{Enabled: true, Users: []string{"+393331234567"}, Groups: []string{group.String()}}
	c := open(t, p, nil)
	c.connected = func() bool { return true }
	for _, ok := range []Origin{{Chat: group.String(), Group: true}, {Chat: owner.String(), Number: "393331234567"}} {
		if err := c.AllowedReply(ok); err != nil {
			t.Errorf("%+v: %v", ok, err)
		}
	}
	for _, bad := range []Origin{{Chat: "1203630999@g.us", Group: true}, {Chat: member.String(), Number: "393339999999"}} {
		if err := c.AllowedReply(bad); err == nil {
			t.Errorf("%+v allowed", bad)
		}
	}
	c.connected = func() bool { return false }
	if err := c.AllowedReply(Origin{Chat: group.String(), Group: true}); !errors.Is(err, ErrNotConnected) {
		t.Errorf("not connected: %v", err)
	}
	if parts := Parts(strings.Repeat("a", 7001)); len(parts) != 3 || len([]rune(parts[2])) != 1 {
		t.Errorf("parts %d", len(parts))
	}
}
