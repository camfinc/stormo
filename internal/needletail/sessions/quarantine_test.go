package sessions

import (
	"errors"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"os"
	"testing"
)

func TestDurableQuarantineMarksTipAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := s.Create(wire.Session{ID: "root"}); err != nil {
		t.Fatal(err)
	}
	tip, err := s.Compact("root", "summary", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUntrusted("root"); err != nil {
		t.Fatal(err)
	}
	next := open(t, dir)
	if !next.Untrusted(tip) {
		t.Fatal("mark lost at restart")
	}
	rows, err := next.Messages(tip)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("synthetic row added: %v", rows)
	}
	if err := next.WithTrusted(tip, func() error { t.Fatal("trusted callback executed"); return nil }); err == nil {
		t.Fatal("marked authority accepted")
	}
	next.fsync = func(*os.File) error { return errors.New("disk failed") }
	if err := next.MarkUntrusted(tip); err == nil {
		t.Fatal("sync failure hidden")
	}
}
