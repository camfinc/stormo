package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// idem.json lives under state/needletail/** (State): its temporary file goes to the home's
// unclaimed temp dir, so a nap never holds a half-written copy.
func TestIdemUsesTheUnclaimedTempDir(t *testing.T) {
	home := t.TempDir()
	tmp := filepath.Join(home, ".needletail-tmp")
	s, err := loadIdem(filepath.Join(home, "state", "needletail"), tmp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.put("k", wire.RunAccepted{RunID: "r"}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "state", "needletail")); len(entries) != 1 || entries[0].Name() != "idem.json" {
		t.Errorf("state dir holds %v", entries)
	}
	_ = os.RemoveAll(tmp)
	_ = os.WriteFile(tmp, nil, 0o600)
	if err := s.put("k2", wire.RunAccepted{RunID: "r2"}); err == nil {
		t.Error("idem.json was written without the temp dir")
	}
}
