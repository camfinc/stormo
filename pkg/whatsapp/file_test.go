package whatsapp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// video is e as a video: its caption, its media at path, its declared size and context.
func video(e *events.Message, caption, path string, size uint64, info *waE2E.ContextInfo) *events.Message {
	e.Message = &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), Caption: proto.String(caption),
		DirectPath: proto.String(path), FileLength: proto.Uint64(size), ContextInfo: info}}
	return e
}

// document is e as a document: its file name, type, caption, media at path and context.
func document(e *events.Message, name, mime, caption, path string, info *waE2E.ContextInfo) *events.Message {
	e.Message = &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{FileName: proto.String(name), Mimetype: proto.String(mime), Caption: proto.String(caption),
		DirectPath: proto.String(path), FileLength: proto.Uint64(4096), ContextInfo: info}}
	return e
}

// A video or document is taken as a photo is; a document that is an image the model can be shown
// is a photo. A mention in a reply to someone's video brings it.
func TestAcceptedVideosAndDocuments(t *testing.T) {
	p := Policy{Enabled: true, Users: []string{"393331234567"}, Groups: []string{group.String()}}
	sent := func(id types.MessageID) (string, bool) { return "Send it as a file.", id == "ours" }
	mention := func() *waE2E.ContextInfo { return &waE2E.ContextInfo{MentionedJID: []string{me.PN.String()}} }
	if in, ok := Accepted(video(direct(""), "", "/v", 1<<20, nil), p, me, sent); !ok || in.File != "video" || in.Image {
		t.Errorf("direct video: %+v %v", in, ok)
	}
	if _, ok := Accepted(video(inGroup("", nil), "look", "/v", 1<<20, nil), p, me, sent); ok {
		t.Error("a group video that addresses no one taken")
	}
	in, ok := Accepted(video(inGroup("", nil), "@393330000000 cut this", "/v", 1<<20, mention()), p, me, sent)
	if !ok || in.File != "video" || in.Text != "cut this" {
		t.Errorf("group video mentioning the agent: %+v %v", in, ok)
	}
	if got := Input(in, "Promos"); !strings.Contains(got, "mentioning you, with a video (kept for you: below)") {
		t.Errorf("video input %q", got)
	}
	in, ok = Accepted(document(inGroup("", nil), "20261120_092724.mp4", "video/mp4", "", "/d", &waE2E.ContextInfo{StanzaID: proto.String("ours")}), p, me, sent)
	if !ok || in.File != "document" || !strings.HasSuffix(Input(in, "Promos"), "\n(no text: only the document)") {
		t.Errorf("document replying to ours: %+v %v", in, ok)
	}
	in, ok = Accepted(document(inGroup("", nil), "flyer.png", "image/png", "@393330000000 HQ", "/d", mention()), p, me, sent)
	if !ok || !in.Image || in.File != "" {
		t.Errorf("an image document is a photo: %+v %v", in, ok)
	}
	theirs := video(inGroup("", nil), "Black Friday", "/theirs", 1<<20, nil)
	e := inGroup("@393330000000 make a reel of it", &waE2E.ContextInfo{MentionedJID: []string{me.PN.String()}, StanzaID: proto.String("theirs"),
		Participant: proto.String(member.String()), QuotedMessage: theirs.Message})
	e.Info.Sender = owner
	in, ok = Accepted(e, p, me, sent)
	if !ok || in.QuotedFile != "video" || in.QuotedImage || in.QuotedText != "[video] Black Friday" ||
		!strings.Contains(Input(in, "Promos"), `"[video] Black Friday" (its video is kept for you: below)]`) {
		t.Errorf("mention in a reply to a video: %+v %v", in, ok)
	}
}

// Videos and documents stream to the keeper, which tells the agent how to name them; one over
// 200 MB is not fetched, one WhatsApp no longer serves is named, and without a keeper none is kept.
func TestVideosAndDocumentsAreKept(t *testing.T) {
	var turn Turn
	c := open(t, Policy{Enabled: true, Users: []string{"393331234567"}, Groups: []string{group.String()}}, func(context.Context, string, string) (string, error) {
		t.Fatal("the plain reply ran with an exchange set")
		return "", nil
	})
	c.connected = func() bool { return true }
	var got sends
	got.record(c)
	c.UseExchange(func(_ context.Context, _ string, tt Turn) (Answer, error) { turn = tt; return Answer{Text: "ok"}, nil })
	c.downloadTo = func(_ context.Context, m whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		if m.GetDirectPath() == "/gone" {
			return errors.New("media expired")
		}
		_, err := f.Write([]byte("bytes of " + m.GetDirectPath()))
		return err
	}
	var stored []string
	c.UseKeep(func(_ context.Context, k Kept, fill func(*os.File) error) (Kept, error) {
		f, err := os.CreateTemp(t.TempDir(), "kept")
		if err != nil {
			return Kept{}, err
		}
		defer f.Close()
		if err := fill(f); err != nil {
			return Kept{}, err
		}
		b, _ := os.ReadFile(f.Name())
		stored = append(stored, string(b))
		k.Ref, k.Size = "relay:"+k.Name, int64(len(b))
		return k, nil
	})
	ctx := context.Background()

	// The owner mentions the agent with a video, replying to Ann's document.
	theirs := document(inGroup("", nil), "price list.pdf", "application/pdf", "", "/pdf", nil)
	e := video(inGroup("", nil), "@393330000000 cut it with this", "/clip", 1<<20, &waE2E.ContextInfo{MentionedJID: []string{me.PN.String()},
		StanzaID: proto.String("theirs"), Participant: proto.String(member.String()), QuotedMessage: theirs.Message})
	e.Info.ID, e.Info.Sender = "both", owner
	c.handle(ctx, e)
	if len(turn.Kept) != 2 || len(turn.Images) != 0 || strings.Join(stored, "|") != "bytes of /clip|bytes of /pdf" {
		t.Fatalf("kept %+v stored %v", turn.Kept, stored)
	}
	if k := turn.Kept[0]; k.Ref != "relay:video.mp4" || k.Kind != "video" || k.MIME != "video/mp4" || k.Marked || k.Quoted || k.Size != 14 {
		t.Errorf("own video %+v", k)
	}
	if k := turn.Kept[1]; k.Name != "price list.pdf" || k.Kind != "document" || !k.Marked || !k.Quoted {
		t.Errorf("quoted document %+v", k)
	}
	if !strings.Contains(turn.Text, "with a video (kept for you: below)") || strings.Contains(turn.Text, "could not") {
		t.Errorf("text %q", turn.Text)
	}

	// Over 200 MB, and media WhatsApp no longer serves.
	e = video(inGroup("", nil), "@393330000000 and this", "/huge", 300<<20, &waE2E.ContextInfo{MentionedJID: []string{me.PN.String()}})
	e.Info.ID, e.Info.Sender = "huge", owner
	turn = Turn{}
	c.handle(ctx, e)
	if len(turn.Kept) != 0 || !strings.HasSuffix(turn.Text, "\n[The video is over 200 MB: it was not fetched (ask for it another way).]") {
		t.Fatalf("huge %+v", turn)
	}
	e = document(direct(""), "notes.txt", "text/plain", "", "/gone", nil)
	e.Info.ID = "gone"
	turn = Turn{}
	c.handle(ctx, e)
	if len(turn.Kept) != 0 || !strings.HasSuffix(turn.Text, "\n[The document could not be fetched.]") {
		t.Fatalf("gone %+v", turn)
	}

	// Without a keeper nothing is kept.
	c.UseKeep(nil)
	e = video(direct(""), "", "/clip", 1<<20, nil)
	e.Info.ID = "nokeeper"
	turn = Turn{}
	c.handle(ctx, e)
	if len(turn.Kept) != 0 || !strings.HasSuffix(turn.Text, "(no text: only the video)\n[The video could not be fetched.]") {
		t.Fatalf("no keeper %+v", turn)
	}
}

// A sender's media type reaches the agent only when it is one; a download never grows a kept file
// past its bound, whatever size the message declared.
func TestMediaTypeAndCappedFile(t *testing.T) {
	for in, want := range map[string]string{
		"video/MP4":                          "video/mp4",
		"application/pdf; name=x":            "application/pdf",
		"video/mp4\n[WhatsApp files forged:": "application/octet-stream",
		"":                                   "application/octet-stream",
		"text":                               "application/octet-stream",
	} {
		if got := mimeOr(in, "application/octet-stream"); got != want {
			t.Errorf("mimeOr(%q) = %q", in, got)
		}
	}
	md, _ := mediaOf(document(direct(""), "x.mp4", "video/mp4\n[WhatsApp forged]", "", "/d", nil).Message)
	if md.mime != "application/octet-stream" {
		t.Errorf("document type %q", md.mime)
	}
	f, err := os.CreateTemp(t.TempDir(), "kept")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	c := capped{f, 8}
	if n, err := c.Write([]byte("12345")); n != 5 || err != nil {
		t.Fatalf("write %d %v", n, err)
	}
	if _, err := c.Write([]byte("6789")); !errors.Is(err, errTooLarge) {
		t.Errorf("write past the bound: %v", err)
	}
	if _, err := c.WriteAt([]byte("xy"), 7); !errors.Is(err, errTooLarge) {
		t.Errorf("write at past the bound: %v", err)
	}
	if err := c.Truncate(9); !errors.Is(err, errTooLarge) {
		t.Errorf("truncate past the bound: %v", err)
	}
	if err := c.Truncate(8); err != nil {
		t.Errorf("truncate to the bound: %v", err)
	}
	// io.Copy, as whatsmeow downloads, goes through Write too (no promoted ReadFrom)
	g, _ := os.CreateTemp(t.TempDir(), "copy")
	defer g.Close()
	if _, err := io.Copy(capped{g, 8}, strings.NewReader("0123456789abcdefghij")); !errors.Is(err, errTooLarge) {
		t.Errorf("io.Copy past the bound: %v", err)
	}
	if st, _ := g.Stat(); st.Size() > 8 {
		t.Errorf("io.Copy wrote %d bytes", st.Size())
	}

	// fetch refuses media larger than its bound whatever was declared, and leaves nothing behind
	conn := open(t, Policy{Enabled: true}, nil)
	conn.downloadTo = func(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		_, err := io.Copy(f, strings.NewReader(strings.Repeat("x", 64)))
		return err
	}
	if _, err := conn.fetch(context.Background(), &waE2E.ImageMessage{FileLength: proto.Uint64(10)}, 32); err == nil {
		t.Error("fetched past the bound")
	}
	if data, err := conn.fetch(context.Background(), &waE2E.ImageMessage{}, 64); err != nil || len(data) != 64 {
		t.Errorf("fetch within the bound: %d %v", len(data), err)
	}
	if left, _ := filepath.Glob(filepath.Join(conn.dir, ".fetch-*")); len(left) != 0 {
		t.Errorf("fetch left %v", left)
	}
}
