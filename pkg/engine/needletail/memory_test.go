package needletail

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/pkg/learning"
)

// The runtime's memory files read the same to the dream and rehydrate (pkg/learning, through the
// engine's Memory format): the same entries, the same budget units. Their entries become the
// ledger's sightings, and the merge at boot keeps them within the same budget the memory tool
// enforces.
func TestMemoryParity(t *testing.T) {
	inst, a := atlas(t)
	f := New(inst).Memory()
	home := t.TempDir()
	store := &memory.Store{Home: home, Limits: map[string]int{memory.TargetMemory: a.Learning.MemoryCharLimit, memory.TargetUser: a.Learning.UserCharLimit}}
	entries := []string{
		"Deals close on Fridays.",
		"  Quotes need a manager's OK.\n",
		"\ufeffA lesson with a BOM in front.",
		"Ends with a no-break space\u00a0",
		"Línea en español: el CRM es lento a las 9 — 🚀 emoji count as two units.",
		"Two lines\nin one entry, and a § that is not a separator.",
		"Next-line char at the end\u0085",
	}
	for _, e := range entries {
		if _, err := store.Add(memory.TargetMemory, e); err != nil {
			t.Fatalf("add %q: %v", e, err)
		}
	}
	if _, err := store.Add(memory.TargetUser, "The owner wants short answers."); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{memory.TargetMemory, memory.TargetUser} {
		rel, _ := memory.File(target)
		raw, err := os.ReadFile(filepath.Join(home, rel))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := store.Entries(target)
		dream := learning.ParseEntries(f, string(raw))
		if !slices.Equal(got, dream) {
			t.Errorf("%s: runtime %q, dream %q", target, got, dream)
		}
		u, _ := store.Current(target)
		if u.Used != learning.UTF16Len(f.File(dream)) {
			t.Errorf("%s: runtime counts %d units, rehydrate %d", target, u.Used, learning.UTF16Len(f.File(dream)))
		}
		// what rehydrate writes back (live entries first) is a file the runtime reads unchanged
		merged, _, _ := learning.MergeHot(f, dream, nil, nil, a.Learning.MemoryCharLimit, a.Learning.SeedFill)
		if err := os.WriteFile(filepath.Join(home, rel), []byte(f.File(merged)), 0o644); err != nil {
			t.Fatal(err)
		}
		if again, _ := store.Entries(target); !slices.Equal(again, got) {
			t.Errorf("%s after rehydrate: %q", target, again)
		}
	}
	if got, _ := store.Entries(memory.TargetMemory); got[2] != "A lesson with a BOM in front." || got[6] != "Next-line char at the end\u0085" {
		t.Errorf("trimmed = %q", got)
	}
}
