package whatsapp

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func voiceNote(chat, sender types.JID, isGroup bool, info *waE2E.ContextInfo, seconds uint32) *events.Message {
	return &events.Message{Info: types.MessageInfo{ID: "in-v", PushName: "Owner", MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsGroup: isGroup}},
		Message: &waE2E.Message{AudioMessage: &waE2E.AudioMessage{Mimetype: proto.String("audio/ogg; codecs=opus"), PTT: proto.Bool(true), Seconds: proto.Uint32(seconds), ContextInfo: info}}}
}

// A voice note is taken from an allowed person, and in a group only as a reply to the agent; one
// over 15 minutes is not.
func TestAcceptedVoiceNotes(t *testing.T) {
	p := Policy{Enabled: true, Users: []string{"393331234567"}, Groups: []string{group.String()}}
	sent := func(id types.MessageID) (string, bool) { return "Booked for the 14th.", id == "ours" }
	if in, ok := Accepted(voiceNote(owner, owner, false, nil, 12), p, me, sent); !ok || !in.Voice || in.Chat != "+393331234567" {
		t.Errorf("direct voice note: %+v %v", in, ok)
	}
	if _, ok := Accepted(voiceNote(owner, owner, false, nil, 16*60), p, me, sent); ok {
		t.Error("a 16-minute voice note taken")
	}
	if _, ok := Accepted(voiceNote(member, member, false, nil, 5), p, me, sent); ok {
		t.Error("a stranger's voice note taken")
	}
	if _, ok := Accepted(voiceNote(group, member, true, nil, 5), p, me, sent); ok {
		t.Error("a group voice note that replies to no one taken")
	}
	in, ok := Accepted(voiceNote(group, member, true, &waE2E.ContextInfo{StanzaID: proto.String("ours")}, 5), p, me, sent)
	if !ok || !in.Voice || in.Quoted != "Booked for the 14th." || in.Verified {
		t.Errorf("group reply voice note: %+v %v", in, ok)
	}
	if got := Input(in, "Trip planning"); !strings.Contains(got, "a voice note: its transcript is below") || !strings.Contains(got, `replying to your message "Booked for the 14th."`) || strings.Contains(got, "mentioning") {
		t.Errorf("input %q", got)
	}
}

// With an exchange, a voice note's audio reaches the agent; its voice answer is posted as a
// push-to-talk Ogg Opus note, then its text when asked; a note that cannot be sent leaves the text.
func TestVoiceNotesBothWays(t *testing.T) {
	var turn Turn
	answer := Answer{Text: "Sí, mañana a las 9.", Voice: oggPage(48000 * 3), WithText: true}
	c := open(t, Policy{Enabled: true, Users: []string{"393331234567"}}, func(context.Context, string, string) (string, error) {
		t.Fatal("the plain reply ran with an exchange set")
		return "", nil
	})
	c.UseExchange(func(_ context.Context, _ string, tt Turn) (Answer, error) { turn = tt; return answer, nil })
	c.download = func(_ context.Context, m whatsmeow.DownloadableMessage) ([]byte, error) {
		return []byte("OggS the person's note"), nil
	}
	var uploads []whatsmeow.MediaType
	c.upload = func(_ context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
		uploads = append(uploads, kind)
		return whatsmeow.UploadResponse{URL: "https://mmg.example/v", DirectPath: "/v", FileLength: uint64(len(data))}, nil
	}
	var notes []*waE2E.AudioMessage
	var texts []string
	failVoice := false
	c.send = func(_ context.Context, _ types.JID, m *waE2E.Message) (types.MessageID, error) {
		if a := m.GetAudioMessage(); a != nil {
			if failVoice {
				return "", errors.New("offline")
			}
			notes = append(notes, a)
			return "note-1", nil
		}
		if m.GetReactionMessage() == nil {
			texts = append(texts, m.GetConversation())
		}
		return "text-1", nil
	}
	c.handle(context.Background(), voiceNote(owner, owner, false, nil, 4))
	if turn.Audio == nil || string(turn.Audio.Data) != "OggS the person's note" || turn.Audio.Marked || !strings.Contains(turn.Text, "voice note") {
		t.Fatalf("turn %+v", turn)
	}
	if len(notes) != 1 || !notes[0].GetPTT() || notes[0].GetMimetype() != "audio/ogg; codecs=opus" || notes[0].GetSeconds() != 3 || len(uploads) != 1 || uploads[0] != whatsmeow.MediaAudio {
		t.Fatalf("notes %+v uploads %v", notes, uploads)
	}
	if strings.Join(texts, "|") != "Sí, mañana a las 9." {
		t.Errorf("texts %v", texts)
	}
	answer.WithText, texts = false, nil
	e := direct("y el viernes?")
	e.Info.ID = "in-2"
	c.handle(context.Background(), e)
	if len(notes) != 2 || len(texts) != 0 {
		t.Errorf("voice only: notes %d texts %v", len(notes), texts)
	}
	failVoice = true
	e = direct("y el sábado?")
	e.Info.ID = "in-3"
	c.handle(context.Background(), e)
	if strings.Join(texts, "|") != "Sí, mañana a las 9." {
		t.Errorf("a note that failed left no text: %v", texts)
	}
}

// oggPage is the start of an Ogg page whose granule position is granule.
func oggPage(granule uint64) []byte {
	b := append([]byte("OggS"), 0, 0)
	g := make([]byte, 8)
	binary.LittleEndian.PutUint64(g, granule)
	return append(append(b, g...), make([]byte, 16)...)
}

func TestOggSeconds(t *testing.T) {
	if s := oggSeconds(append(oggPage(480), oggPage(48000*61+1)...)); s != 62 {
		t.Errorf("seconds %d", s)
	}
	if oggSeconds([]byte("not ogg")) != 0 {
		t.Error("seconds from no Ogg")
	}
}
