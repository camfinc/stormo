package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func TestAccepted(t *testing.T) {
	message := func() *events.Message {
		return &events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: types.NewJID("393331234567", types.DefaultUserServer), Sender: types.NewJID("393331234567", types.DefaultUserServer)}}, Message: &waE2E.Message{Conversation: proto.String("hello")}}
	}
	policy := Policy{Enabled: true, Users: []string{"+393331234567"}}
	for _, test := range []struct {
		name   string
		modify func(*events.Message, *Policy)
		want   bool
	}{
		{"allowed", func(*events.Message, *Policy) {}, true},
		{"denied", func(_ *events.Message, p *Policy) { p.Users = nil }, false},
		{"disabled", func(_ *events.Message, p *Policy) { p.Enabled = false }, false},
		{"own", func(e *events.Message, _ *Policy) { e.Info.IsFromMe = true }, false},
		{"group", func(e *events.Message, _ *Policy) { e.Info.IsGroup = true }, false},
		{"media", func(e *events.Message, _ *Policy) { e.Message = &waE2E.Message{} }, false},
		{"lid", func(e *events.Message, _ *Policy) {
			e.Info.SenderAlt = e.Info.Sender
			e.Info.Sender = types.NewJID("555", types.HiddenUserServer)
		}, true},
		{"unknown lid", func(e *events.Message, _ *Policy) { e.Info.Sender = types.NewJID("555", types.HiddenUserServer) }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := message()
			p := policy
			test.modify(e, &p)
			_, got := Accepted(e, p)
			if got != test.want {
				t.Fatalf("accepted=%v", got)
			}
		})
	}
}
func TestSessionPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account", "session.db")
	open := func() *Connector {
		c, err := Open(path, func() Policy { return Policy{} }, func(context.Context, string, string) (string, error) {
			t.Fatal("unexpected agent invocation")
			return "", nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	if c.Status().State != "disconnected" || c.Paired() {
		t.Fatal("unexpected initial state")
	}
	if _, err := c.db.Exec(`INSERT INTO stormo_received VALUES ('message',1)`); err != nil {
		t.Fatal(err)
	}
	c.Close()
	c = open()
	defer c.Close()
	r, err := c.db.Exec(`INSERT OR IGNORE INTO stormo_received VALUES ('message',2)`)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := r.RowsAffected()
	if n != 0 {
		t.Fatal("duplicate lost across restart")
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("credential permissions %v", stat.Mode())
	}
}

func TestDeliveryAndDeduplication(t *testing.T) {
	calls, sends := 0, 0
	c, err := Open(filepath.Join(t.TempDir(), "session.db"), func() Policy { return Policy{Enabled: true, Users: []string{"393331234567"}} }, func(_ context.Context, session, text string) (string, error) {
		calls++
		if text != "hello" || len(session) == 0 {
			t.Fatal("lost conversation input")
		}
		return "reply", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.send = func(_ context.Context, _ types.JID, text string) error {
		sends++
		if text != "reply" {
			t.Fatal(text)
		}
		return nil
	}
	e := &events.Message{Info: types.MessageInfo{ID: "unique", MessageSource: types.MessageSource{Sender: types.NewJID("393331234567", types.DefaultUserServer), Chat: types.NewJID("393331234567", types.DefaultUserServer)}}, Message: &waE2E.Message{Conversation: proto.String("hello")}}
	c.handle(context.Background(), e)
	c.handle(context.Background(), e)
	if calls != 1 || sends != 1 {
		t.Fatalf("calls=%d sends=%d", calls, sends)
	}
}
