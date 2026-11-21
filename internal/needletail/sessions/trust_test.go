package sessions

import (
	"errors"
	"path/filepath"
	"testing"

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

// A message a conversation sent while marked is covered by an endorsement of that conversation
// made since (the core asks before it clears the message's mark): the session live then, or a
// compaction's continuation of it, endorsed after the send. Never an endorsement from before it,
// nor a /new's fresh session, nor a conversation that started later.
func TestEndorsedSince(t *testing.T) {
	s := open(t, t.TempDir())
	id := "whatsapp-0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	if _, err := s.Create(wire.Session{ID: id, Source: "whatsapp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaint(id, wire.Taint{Why: "web page"}); err != nil {
		t.Fatal(err)
	}
	sent := s.now() // a marked message leaves the conversation
	if _, _, ok := s.EndorsedSince(id, sent); ok {
		t.Fatal("endorsed before any trust")
	}
	if err := s.Trust(id, "owner (API)"); err != nil {
		t.Fatal(err)
	}
	if by, when, ok := s.EndorsedSince(id, sent); !ok || by != "owner (API)" || when == "" {
		t.Fatalf("trust after the send: %q %q %v", by, when, ok)
	}
	if _, _, ok := s.EndorsedSince(id, s.now()); ok {
		t.Fatal("a trust from before the send covers it")
	}
	// marked again, a message, compacted: endorsing the continuation covers it, asked by either id
	if err := s.AddTaint(id, wire.Taint{Why: "read a swarm message from nova that carries untrusted input"}); err != nil {
		t.Fatal(err)
	}
	sent = s.now()
	tip, err := s.CompactMarked(id, "summary", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trust(tip, "screen:decision.ask"); err != nil {
		t.Fatal(err)
	}
	for _, ask := range []string{id, tip} {
		if by, _, ok := s.EndorsedSince(ask, sent); !ok || by != "screen:decision.ask" {
			t.Fatalf("compaction continued (asked as %s): %q %v", ask, by, ok)
		}
	}
	// marked again, a message, then /new: the fresh session's trust never vouches for it
	if err := s.AddTaint(tip, wire.Taint{Why: "web page"}); err != nil {
		t.Fatal(err)
	}
	sent = s.now()
	fresh, err := s.Restart(tip, "fresh start")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Trust(fresh, "owner (API)"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.EndorsedSince(id, sent); ok {
		t.Fatal("a /new's trust covers what the conversation held before it")
	}
	// unknown, or asked about a time before the conversation began
	if _, _, ok := s.EndorsedSince("whatsapp-nobody", sent); ok {
		t.Fatal("unknown conversation endorsed")
	}
	later := "slack-D0DM000002-dm"
	if _, err := s.Create(wire.Session{ID: later, Source: "slack"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Trust(later, "U0OWNER001"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.EndorsedSince(later, sent); ok {
		t.Fatal("a conversation that began after the send covers it")
	}
}
