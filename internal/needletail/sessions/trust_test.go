package sessions

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Item 5 (Codex 5, Claude M3): an approval's "trust thread" endorses the conversation as it stood
// at the request, never data that came in after it: a mark added since (a read of marked data,
// another turn, an unverified author), a compaction or a restart refuses the trust part.
func TestTrustAtTheRequest(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(wire.Session{ID: "slack-C0DEV00001-1.1", Source: "slack"}); err != nil {
		t.Fatal(err)
	}
	id := "slack-C0DEV00001-1.1"
	if err := s.AddTaint(id, wire.Taint{Why: "an unverified author", Users: []string{"U0MALLORY1"}}); err != nil {
		t.Fatal(err)
	}
	p, ok := s.Point(id)
	if !ok {
		t.Fatal("no point")
	}
	// the requesting turn's own rows (no new mark) are endorsed with it
	if _, err := s.Append(id, wire.Message{Role: "assistant", Content: "pending approval", Untrusted: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.TrustAt(id, "U0OWNER001", p); err != nil || s.Untrusted(id) {
		t.Fatalf("trust at the request: %v (untrusted %v)", err, s.Untrusted(id))
	}
	// marked again; a request; then more marked data: the trust part is refused
	if err := s.AddTaint(id, wire.Taint{Why: "an unverified author", Users: []string{"U0MALLORY1"}}); err != nil {
		t.Fatal(err)
	}
	p, _ = s.Point(id)
	if err := s.AddTaint(id, wire.Taint{Why: "read a memory record saved from untrusted input"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TrustAt(id, "U0OWNER001", p); !errors.Is(err, ErrTrustMoved) || !s.Untrusted(id) {
		t.Fatalf("trust past new taint: %v (untrusted %v)", err, s.Untrusted(id))
	}
	// a point of another process (a restart) is refused too
	p, _ = s.Point(id)
	p.Epoch = "another"
	if err := s.TrustAt(id, "U0OWNER001", p); !errors.Is(err, ErrTrustMoved) {
		t.Fatalf("trust across a restart: %v", err)
	}
}

// A message a conversation sent while marked is covered by an endorsement that saw the model's
// own send: a session of the conversation holding the call within its endorsed rows (where it
// was made, or a compaction's continuation that carried it). Never a send after the
// endorsement, a message merely read (even answering a reused call id), a conversation that never
// sent it, an id another message reused later, a summary of the send, nor a /new's fresh session.
func TestEndorsedSend(t *testing.T) {
	s := open(t, t.TempDir())
	id := "whatsapp-0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	if _, err := s.Create(wire.Session{ID: id, Source: "whatsapp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaint(id, wire.Taint{Why: "web page"}); err != nil {
		t.Fatal(err)
	}
	send := func(session, call string, message int64) time.Time {
		t.Helper()
		rows, err := s.Append(session,
			wire.Message{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: call, Name: SendTool, Arguments: `{"to":"nova","subject":"s","body":"b"}`}}, Untrusted: true},
			wire.Message{Role: "tool", ToolCallID: call, Content: fmt.Sprintf(`{"id":%d,"thread":%d,"recipients":["nova"]}`, message, message), Untrusted: true})
		if err != nil {
			t.Fatal(err)
		}
		return parseStamp(rows[1].At)
	}
	endorsed := func(session string, message int64, at time.Time) (string, bool) {
		t.Helper()
		by, ok, err := s.EndorsedSend(context.Background(), session, message, at)
		if err != nil {
			t.Fatal(err)
		}
		return by, ok
	}
	at7 := send(id, "c7", 7)
	if _, ok := endorsed(id, 7, at7); ok {
		t.Fatal("endorsed before any trust")
	}
	// a message it only read names an id too, even when the provider used the send's call id again
	if _, err := s.Append(id, wire.Message{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "c7", Name: "mcp_core_msg_read", Arguments: `{"id":9}`}}},
		wire.Message{Role: "tool", ToolCallID: "c7", Content: `{"id":9,"from":"nova","untrusted":"web page"}`, Untrusted: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Trust(id, "owner (API)"); err != nil {
		t.Fatal(err)
	}
	if by, ok := endorsed(id, 7, at7); !ok || by != "owner (API)" {
		t.Fatalf("a send the trust saw: %q %v", by, ok)
	}
	if _, ok := endorsed(id, 9, at7); ok {
		t.Fatal("a message the conversation only read")
	}
	if _, ok := endorsed(id, 7, at7.Add(2*time.Hour)); ok {
		t.Fatal("a later message that reused the id")
	}
	// another conversation, endorsed, never sent it
	other := "slack-D0DM000002-dm"
	if _, err := s.Create(wire.Session{ID: other, Source: "slack"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Trust(other, "U0OWNER001"); err != nil {
		t.Fatal(err)
	}
	if _, ok := endorsed(other, 7, at7); ok {
		t.Fatal("a conversation that never sent it vouches for it")
	}
	// marked again: a send after the trust is not covered by it
	if err := s.AddTaint(id, wire.Taint{Why: "read a swarm message from nova that carries untrusted input"}); err != nil {
		t.Fatal(err)
	}
	at8 := send(id, "c8", 8)
	if _, ok := endorsed(id, 8, at8); ok {
		t.Fatal("a trust from before the send covers it")
	}
	// compacted with only a summary of it: endorsing the continuation never saw the words sent
	tip, err := s.CompactMarked(id, "summary", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trust(tip, "screen:decision.ask"); err != nil {
		t.Fatal(err)
	}
	if _, ok := endorsed(id, 8, at8); ok {
		t.Fatal("a summary vouches for the send it summarised")
	}
	// a send the next compaction carries into its rows: the continuation's endorsement saw it
	if err := s.AddTaint(tip, wire.Taint{Why: "web page"}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Append(tip,
		wire.Message{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "c10", Name: SendTool, Arguments: `{"to":"nova"}`}}, Untrusted: true},
		wire.Message{Role: "tool", ToolCallID: "c10", Content: `{"id":10,"thread":10}`, Untrusted: true})
	if err != nil {
		t.Fatal(err)
	}
	at10 := parseStamp(rows[1].At)
	later := s.now
	s.now = func() time.Time { return later().Add(2 * time.Hour) } // compacted hours after the send
	carried, err := s.CompactMarked(tip, "summary", true, rows)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := endorsed(id, 10, at10); ok {
		t.Fatal("covered before the continuation was endorsed")
	}
	if err := s.Trust(carried, "screen:decision.ask"); err != nil {
		t.Fatal(err)
	}
	for _, ask := range []string{id, carried} {
		if by, ok := endorsed(ask, 10, at10); !ok || by != "screen:decision.ask" {
			t.Fatalf("carried into the continuation (asked as %s): %q %v", ask, by, ok)
		}
	}
	tip = carried
	// marked again, a send, then /new: the fresh session's trust never vouches for it
	if err := s.AddTaint(tip, wire.Taint{Why: "web page"}); err != nil {
		t.Fatal(err)
	}
	at11 := send(tip, "c11", 11)
	fresh, err := s.Restart(tip, "fresh start")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trust(fresh, "owner (API)"); err != nil {
		t.Fatal(err)
	}
	if _, ok := endorsed(id, 11, at11); ok {
		t.Fatal("a /new's trust covers what the conversation held before it")
	}
	if _, ok := endorsed("whatsapp-nobody", 7, at7); ok {
		t.Fatal("unknown conversation endorsed")
	}
}
