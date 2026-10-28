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
