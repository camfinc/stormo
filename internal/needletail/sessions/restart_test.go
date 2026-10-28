package sessions

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// /new (docs/needletail.md §4.9): the conversation goes on in a fresh child session of its live
// one, unmarked and with none of the earlier rows; the id, its root and its lineage stay, the
// earlier sessions keep their rows and marks, and the restart survives a reopen.
func TestRestart(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sessions")
	s, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := "slack-D0DM000001-dm"
	if _, err := s.Create(wire.Session{ID: id, Source: "slack", Title: "Slack conversation"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(id, wire.Message{Role: "user", Content: "read this page", Untrusted: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddTaint(id, wire.Taint{Why: "web page", Users: []string{"U0MALLORY1"}}); err != nil {
		t.Fatal(err)
	}
	if !s.Untrusted(id) {
		t.Fatal("not marked before /new")
	}
	tip, err := s.Restart(id, "fresh start")
	if err != nil {
		t.Fatal(err)
	}
	if tip == id {
		t.Fatal("no new session")
	}
	if r, _ := s.Root(tip); r != id {
		t.Fatalf("root %q, want %q", r, id)
	}
	if got, _ := s.Tip(id); got != tip {
		t.Fatalf("tip %q, want %q", got, tip)
	}
	if s.Untrusted(id) || len(s.Taint(id)) != 0 {
		t.Fatalf("still marked after /new: %v %v", s.Untrusted(id), s.Taint(id))
	}
	rows, _ := s.Messages(tip)
	if len(rows) != 1 || rows[0].Role != "system" || rows[0].Content != "fresh start" {
		t.Fatalf("new session rows %+v", rows)
	}
	if h, _ := s.Get(id); h.EndReason != wire.EndNew || !h.Untrusted {
		t.Fatalf("old session %+v: want ended %q and still marked", h, wire.EndNew)
	}
	if h, _ := s.Get(tip); h.Source != "slack" || h.Title != "Slack conversation" {
		t.Fatalf("new session header %+v", h)
	}
	// an unknown conversation is refused
	if _, err := s.Restart("no-such", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	s2, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s2.Tip(id); got != tip || s2.Untrusted(id) {
		t.Fatalf("after reopen: tip %q untrusted %v", got, s2.Untrusted(id))
	}
	// a second /new goes on from the new tip
	tip2, err := s2.Restart(tip, "")
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := s2.Lineage(id); len(l) != 3 || l[2] != tip2 {
		t.Fatalf("lineage %v", l)
	}
	if rows, _ := s2.Messages(tip2); len(rows) != 0 {
		t.Fatalf("no note: rows %+v", rows)
	}
}
