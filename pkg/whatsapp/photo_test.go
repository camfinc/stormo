package whatsapp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// photo is e as a photo: its caption, its media at path, its context (mentions, a quoted message).
func photo(e *events.Message, caption, path string, info *waE2E.ContextInfo) *events.Message {
	e.Message = &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), Caption: proto.String(caption),
		DirectPath: proto.String(path), FileLength: proto.Uint64(2048), ContextInfo: info}}
	return e
}

// A photo is taken as a text is: direct from an allowed person; in a group when its caption
// mentions the agent or it replies to one of its messages. A mention in a reply to someone else's
// photo brings that photo.
func TestAcceptedPhotos(t *testing.T) {
	p := Policy{Enabled: true, Users: []string{"393331234567"}, Groups: []string{group.String()}}
	sent := func(id types.MessageID) (string, bool) { return "Send it as a file.", id == "ours" }
	if in, ok := Accepted(photo(direct(""), "", "/own", nil), p, me, sent); !ok || !in.Image || in.Text != "" || in.Chat != "+393331234567" {
		t.Errorf("direct photo: %+v %v", in, ok)
	}
	if _, ok := Accepted(photo(direct(""), "", "/own", nil), Policy{Enabled: true}, me, sent); ok {
		t.Error("a stranger's photo taken")
	}
	if _, ok := Accepted(photo(inGroup("", nil), "look", "/own", nil), p, me, sent); ok {
		t.Error("a group photo that addresses no one taken")
	}
	if _, ok := Accepted(photo(inGroup("", nil), "@393331234567 look", "/own", &waE2E.ContextInfo{MentionedJID: []string{owner.String()}}), p, me, sent); ok {
		t.Error("a group photo mentioning someone else taken")
	}
	in, ok := Accepted(photo(inGroup("", nil), "@393330000000 redo this", "/own", &waE2E.ContextInfo{MentionedJID: []string{me.PN.String()}}), p, me, sent)
	if !ok || !in.Image || in.Text != "redo this" {
		t.Errorf("group photo mentioning the agent: %+v %v", in, ok)
	}
	in, ok = Accepted(photo(inGroup("", nil), "", "/own", &waE2E.ContextInfo{StanzaID: proto.String("ours")}), p, me, sent)
	if !ok || !in.Image || in.Quoted != "Send it as a file." {
		t.Errorf("group photo replying to ours: %+v %v", in, ok)
	}
	if got := Input(in, "Promos"); !strings.Contains(got, `replying to your message "Send it as a file.", with a photo (attached below)`) || !strings.HasSuffix(got, "\n(no text: only the photo)") {
		t.Errorf("photo input %q", got)
	}
	theirs := photo(inGroup("", nil), "Black Friday", "/theirs", nil)
	e := inGroup("@393330000000 redo it in high quality", &waE2E.ContextInfo{MentionedJID: []string{me.PN.String()}, StanzaID: proto.String("theirs"),
		Participant: proto.String(member.String()), QuotedMessage: theirs.Message})
	e.Info.Sender = owner
	in, ok = Accepted(e, p, me, sent)
	if !ok || in.Image || !in.QuotedImage || in.QuotedText != "[image] Black Friday" {
		t.Errorf("mention in a reply to a photo: %+v %v", in, ok)
	}
	if got := Input(in, "Promos"); !strings.HasSuffix(got, `not one of your allowed people: "[image] Black Friday" (its photo is attached below)]`+"\nredo it in high quality") {
		t.Errorf("quoted photo input %q", got)
	}
}

// With an exchange, a photo reaches the agent as a file, and so does the photo a mention replies
// to, each marked when whoever sent it may not instruct the agent; one that cannot be fetched is
// named in the input instead. Without one, the input says the photo could not be given.
func TestPhotosReachTheAgent(t *testing.T) {
	var turn Turn
	c := open(t, Policy{Enabled: true, Users: []string{"393331234567"}, Groups: []string{group.String()}}, func(context.Context, string, string) (string, error) {
		t.Fatal("the plain reply ran with an exchange set")
		return "", nil
	})
	c.connected = func() bool { return true }
	var got sends
	got.record(c)
	c.UseExchange(func(_ context.Context, _ string, tt Turn) (Answer, error) { turn = tt; return Answer{Text: "ok"}, nil })
	c.download = func(_ context.Context, m whatsmeow.DownloadableMessage) ([]byte, error) {
		if m.GetDirectPath() == "/gone" {
			return nil, errors.New("media expired")
		}
		return []byte("jpeg " + m.GetDirectPath()), nil
	}
	ctx := context.Background()
	image := func(i int, name, data string, marked bool) bool {
		return i < len(turn.Images) && turn.Images[i].Name == name && string(turn.Images[i].Data) == data && turn.Images[i].MIME == "image/jpeg" && turn.Images[i].Marked == marked
	}

	// The owner mentions the agent in a reply to Ann's photo: it comes, marked (Ann may not instruct).
	theirs := photo(inGroup("", nil), "Black Friday", "/theirs", nil)
	e := inGroup("@393330000000 redo it in high quality", &waE2E.ContextInfo{MentionedJID: []string{me.PN.String()}, StanzaID: proto.String("theirs"),
		Participant: proto.String(member.String()), QuotedMessage: theirs.Message})
	e.Info.ID, e.Info.Sender = "ask", owner
	c.handle(ctx, e)
	if len(turn.Images) != 1 || !image(0, "quoted-photo.jpeg", "jpeg /theirs", true) || !strings.Contains(turn.Text, "(its photo is attached below)") || strings.Contains(turn.Text, "could not be fetched") {
		t.Fatalf("quoted photo turn %+v", turn)
	}

	// The owner answers the agent's message with the photo alone: it comes, unmarked.
	if _, err := c.Send(ctx, group, "Send it again as a file."); err != nil {
		t.Fatal(err)
	}
	e = photo(inGroup("", nil), "", "/own", &waE2E.ContextInfo{StanzaID: proto.String("sent-" + group.User), Participant: proto.String(me.PN.String())})
	e.Info.ID, e.Info.Sender = "resent", owner
	turn = Turn{}
	c.handle(ctx, e)
	if len(turn.Images) != 1 || !image(0, "photo.jpeg", "jpeg /own", false) || !strings.HasSuffix(turn.Text, "\n(no text: only the photo)") {
		t.Fatalf("photo turn %+v", turn)
	}

	// A photo that mentions the agent, replying to a photo WhatsApp no longer serves: its own comes,
	// the other is named.
	gone := photo(inGroup("", nil), "", "/gone", nil)
	e = photo(inGroup("", nil), "@393330000000 and this one?", "/own2", &waE2E.ContextInfo{MentionedJID: []string{me.PN.String()}, StanzaID: proto.String("old"),
		Participant: proto.String(member.String()), QuotedMessage: gone.Message})
	e.Info.ID, e.Info.Sender = "both", owner
	turn = Turn{}
	c.handle(ctx, e)
	if len(turn.Images) != 1 || !image(0, "photo.jpeg", "jpeg /own2", false) || !strings.HasSuffix(turn.Text, "and this one?\n[The photo replied to could not be fetched.]") {
		t.Fatalf("photo with a lost quoted one %+v", turn)
	}

	var input string
	plain := open(t, Policy{Enabled: true, Users: []string{"393331234567"}}, func(_ context.Context, _, text string) (string, error) {
		input = text
		return "ok", nil
	})
	plain.connected = func() bool { return true }
	var none sends
	none.record(plain)
	plain.handle(ctx, photo(direct(""), "redo this", "/own", nil))
	if !strings.HasSuffix(input, "redo this\n[The photo could not be given to you: this connection takes text only.]") {
		t.Fatalf("plain input %q", input)
	}
}
