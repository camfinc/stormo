package needletail

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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
		// the hot file is the rendering of the records marked hot, in order
		recs, _ := os.ReadFile(filepath.Join(home, filepath.FromSlash(memory.RecordsFile)))
		hot := map[int]string{}
		for _, r := range memory.ParseRecords(string(recs), time.Now()).Recs {
			if r.Hot > 0 && r.Target() == target {
				hot[r.Hot] = r.Text
			}
		}
		for i, e := range got {
			if hot[i+1] != e {
				t.Errorf("%s: hot record %d is %q, the file has %q", target, i+1, hot[i+1], e)
			}
		}
	}
	if got, _ := store.Entries(memory.TargetMemory); got[2] != "A lesson with a BOM in front." || got[6] != "Next-line char at the end\u0085" {
		t.Errorf("trimmed = %q", got)
	}
}

// The records' copies of pkg/learning agree with it: Normalize (dedupe and the merge at boot match
// entries the way the ledger does) and the generic PII rules (the flags a record shows are the
// kinds the dream's scrub finds, without the instance's own rules).
func TestRecordsParity(t *testing.T) {
	inst, _ := atlas(t)
	scrub := learning.NewScrubber(inst)
	for _, s := range []string{
		"  Deals close on Fridays.\n", "TWO\u00a0spaces\u2003and\ttabs\n\nhere", "\ufeffBOM", "Línea Ñandú", "",
		"Call +1 415 555 0123", "mail ana@example.com", "card 4111 1111 1111 1111", "token sk_abcdefghijklmnop", "Bearer abcdefghijklmnopqrst",
		"123456789:AAabcdefghijklmnopqrstuvwxyz0123456", "booking ZX-4471 on 2026-10-20", "no pii at all",
		"bad \xff byte\u2001\u2001end ", "\u0085next line\u200bzero width", "ÀÉÎ STRASSE ǅ",
	} {
		if memory.Normalize(s) != learning.Normalize(s) {
			t.Errorf("Normalize(%q): runtime %q, ledger %q", s, memory.Normalize(s), learning.Normalize(s))
		}
		_, kinds := scrub.Scrub(s)
		got := memory.PII(s)
		if len(kinds) == 0 {
			kinds = nil
		}
		if !slices.Equal(got, kinds) {
			t.Errorf("PII(%q): runtime %v, scrubber %v", s, got, kinds)
		}
	}
}
