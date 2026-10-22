package memory

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A hot file whose entry has a bare § line at its edge (an empty entry between two delimiters,
// a § first or last in the file) never reaches RECORDS.md as such: the line is dropped, the hot
// file rewritten, and boot after boot the records stay what they are.
func TestBareSectionLinesInAHotFile(t *testing.T) {
	for _, mem := range []string{"A.\n§\n§\nB.", "§\nA.\n§\nB.", "A.\n§\nB.\n§", "A.\n§\n§\n§\nB."} {
		home := newHome(t, mem, "")
		for boot := 0; boot < 3; boot++ {
			if err := newStore(home).Open(); err != nil {
				t.Fatal(err)
			}
			p := ParseRecords(read(t, home, RecordsFile), t0)
			if len(p.Bad) != 0 || len(p.Recs) != 2 || p.Repaired != 0 {
				t.Fatalf("%q, boot %d: %d records, bad %q, repaired %d", mem, boot, len(p.Recs), p.Bad, p.Repaired)
			}
			got := []string{p.Recs[0].Text, p.Recs[1].Text}
			if !slices.Equal(got, []string{"A.", "B."}) {
				t.Errorf("%q: texts %q", mem, got)
			}
			if f := read(t, home, "memories/MEMORY.md"); f != "A.\n§\nB." {
				t.Errorf("%q: MEMORY.md = %q", mem, f)
			}
		}
	}
}

// FormatRecords never writes a text that does not read back as itself: a bare § line in a text
// (which no entry point lets in) is escaped.
func TestFormatRecordsRoundTrips(t *testing.T) {
	texts := []string{"§\nstarts", "ends\n§", "mid\n§\ndle", "§", "x\n§\n§\ny", "§ not bare\n §"}
	recs := []*Record{}
	for i, text := range texts {
		recs = append(recs, &Record{ID: "m00000000" + string(rune('a'+i)) + "00", Kind: KindFact, Tags: []string{}, Imp: 3, Src: "boot",
			Created: "2026-10-01T00:00:00Z", Updated: "2026-10-01T00:00:00Z", Text: text})
	}
	out := FormatRecords(recs, nil)
	p := ParseRecords(out, t0)
	if len(p.Bad) != 0 || len(p.Recs) != len(texts) || p.Repaired != 0 {
		t.Fatalf("%d records, bad %q:\n%s", len(p.Recs), p.Bad, out)
	}
	for i, r := range p.Recs {
		if r.ID != recs[i].ID || strings.Contains("\n"+r.Text+"\n", Delimiter) {
			t.Errorf("record %d = %q", i, r.Text)
		}
	}
	if again := ParseRecords(FormatRecords(p.Recs, nil), t0); FormatRecords(again.Recs, nil) != FormatRecords(p.Recs, nil) {
		t.Error("not stable")
	}
	if p.Recs[5].Text != "§ not bare\n §" {
		t.Errorf("a § that is not a line of its own changed: %q", p.Recs[5].Text)
	}
}

// A RECORDS.md an earlier runtime damaged (a record whose text began with a bare § line: its
// header and its text read as two bad entries, one more pair each boot) is repaired on load: the
// pairs are records again, the copies the boots made dropped, and the file rewritten.
func TestDamagedRecordsRepaired(t *testing.T) {
	home := newHome(t, "A.\n§\n§\nB.", "")
	hdr := func(id string, hot int) string {
		r := &Record{ID: id, Kind: KindFact, Tags: []string{}, Imp: 3, Hot: hot, Src: "boot", Created: "2026-10-01T00:00:00Z", Updated: "2026-10-01T00:00:00Z"}
		var b bytes.Buffer
		r.header(&b)
		return b.String()
	}
	damaged := versionLine + Delimiter +
		hdr("m00000000001", 1) + "\nA." + Delimiter +
		hdr("m00000000002", 2) + Delimiter + "B." + Delimiter +
		hdr("m00000000003", 2) + Delimiter + "B." + Delimiter +
		`<!-- {"id":"m00000000004","kind":"fact","tags":[],"imp":3,"hot":0,"src":"chat:s","created":"2026-10-01T00:00:00Z","updated":"2026-10-01T00:00:00Z"} -->` + "\nKept.\n§\n" +
		"§\n" + hdr("m00000000005", 0) + "\nAfter a stray line." + Delimiter + "§\n"
	_ = os.WriteFile(filepath.Join(home, filepath.FromSlash(RecordsFile)), []byte(damaged), 0o640)
	p := ParseRecords(damaged, t0)
	if p.Repaired == 0 {
		t.Fatalf("nothing repaired: %+v", p)
	}
	if err := newStore(home).Open(); err != nil {
		t.Fatal(err)
	}
	p = ParseRecords(read(t, home, RecordsFile), t0)
	got := map[string]int{}
	for _, r := range p.Recs {
		got[r.ID+" "+r.Text] = r.Hot
	}
	want := map[string]int{"m00000000001 A.": 1, "m00000000002 B.": 2, "m00000000004 Kept.": 0, "m00000000005 After a stray line.": 0}
	if len(p.Bad) != 0 || len(got) != len(want) {
		t.Fatalf("records %v, bad %q", got, p.Bad)
	}
	for k, hot := range want {
		if h, ok := got[k]; !ok || h != hot {
			t.Errorf("%s: %v %d", k, ok, h)
		}
	}
	if f := read(t, home, "memories/MEMORY.md"); f != "A.\n§\nB." {
		t.Errorf("MEMORY.md = %q", f)
	}
}

// The repair drops only a boot's (or the migration's) redundant copy of a mended record: one of
// the same target, kind, scope, tags and expiry. A record with the same text but its own scope,
// source or metadata is a distinct record, and both survive: Ana's damaged record beside Bob's
// intact one, a boot copy with tags of its own, one beside an unscoped chat record (whose scope
// its src gives).
func TestRepairKeepsDistinctRecordsSharingText(t *testing.T) {
	hdr := func(r Record) string {
		if r.Tags == nil {
			r.Tags = []string{}
		}
		r.Kind, r.Imp, r.Created, r.Updated = KindFact, 3, "2026-10-01T00:00:00Z", "2026-10-01T00:00:00Z"
		var b bytes.Buffer
		r.header(&b)
		return b.String()
	}
	const text = "The client's passport expires in March 2027."
	damaged := versionLine + Delimiter +
		hdr(Record{ID: "m000000000b1", Src: "chat:slack-DBOB-dm", Scope: "dm:UBOB"}) + "\n" + text + Delimiter +
		hdr(Record{ID: "m000000000a1", Src: "chat:slack-DANA-dm", Scope: "dm:UANA"}) + Delimiter + text + Delimiter +
		hdr(Record{ID: "m000000000c1", Src: "boot", Scope: "dm:UBOB"}) + Delimiter + text + Delimiter + // a copy of Bob's: dropped
		hdr(Record{ID: "m000000000c2", Src: "boot", Scope: "dm:UBOB", Tags: []string{"travel"}}) + Delimiter + text + Delimiter +
		hdr(Record{ID: "m000000000c3", Src: "boot", Scope: "dm:UBOB", Expires: "2027-03-31"}) + Delimiter + text + Delimiter +
		hdr(Record{ID: "m000000000c4", Src: "migration", Scope: "agent"}) + Delimiter + text + Delimiter +
		hdr(Record{ID: "m000000000d1", Src: "chat:slack-DCAT-dm"}) + "\nCall the hotel at noon." + Delimiter +
		hdr(Record{ID: "m000000000d2", Src: "boot"}) + Delimiter + "Call the hotel at noon." + Delimiter +
		hdr(Record{ID: "m000000000e1", Src: "luau:x"}) + "\nThe office opens at nine." + Delimiter +
		hdr(Record{ID: "m000000000e2", Src: "boot"}) + Delimiter + "The office opens at nine." // a copy: dropped
	want := []string{"m000000000b1", "m000000000a1", "m000000000c2", "m000000000c3", "m000000000c4", "m000000000d1", "m000000000d2", "m000000000e1"}
	ids := func(p Records) []string {
		var out []string
		for _, r := range p.Recs {
			out = append(out, r.ID)
		}
		return out
	}
	p := ParseRecords(damaged, t0)
	if got := ids(p); !slices.Equal(got, want) || len(p.Bad) != 0 {
		t.Fatalf("records %v, want %v (bad %q)", got, want, p.Bad)
	}
	home := newHome(t, "", "")
	_ = os.WriteFile(filepath.Join(home, filepath.FromSlash(RecordsFile)), []byte(damaged), 0o640)
	if err := newStore(home).Open(); err != nil {
		t.Fatal(err)
	}
	again := ParseRecords(read(t, home, RecordsFile), t0)
	if got := ids(again); !slices.Equal(got, want) || again.Repaired != 0 {
		t.Errorf("after the rewrite: %v, repaired %d", got, again.Repaired)
	}
	for _, r := range again.Recs {
		if r.ID == "m000000000a1" && (r.Scope != "dm:UANA" || r.Text != text) {
			t.Errorf("Ana's record = %+v", r)
		}
	}
}

// A repeated id is renamed once and saved: the next boot reads the same ids.
func TestRenamedIDsAreSaved(t *testing.T) {
	home := newHome(t, "", "")
	body := versionLine + Delimiter +
		`<!-- {"id":"m00000000001","kind":"fact","tags":[],"imp":3,"hot":0,"src":"chat:s","created":"2026-10-01T00:00:00Z","updated":"2026-10-01T00:00:00Z"} -->` + "\nFirst." + Delimiter +
		`<!-- {"id":"m00000000001","kind":"fact","tags":[],"imp":3,"hot":0,"src":"chat:s","created":"2026-10-01T00:00:00Z","updated":"2026-10-01T00:00:00Z"} -->` + "\nSecond.\n"
	_ = os.WriteFile(filepath.Join(home, filepath.FromSlash(RecordsFile)), []byte(body), 0o640)
	if err := newStore(home).Open(); err != nil {
		t.Fatal(err)
	}
	first := ParseRecords(read(t, home, RecordsFile), t0)
	if first.Renamed != 0 || len(first.Recs) != 2 || first.Recs[0].ID == first.Recs[1].ID {
		t.Fatalf("the rename was not saved: %+v", first.Recs)
	}
	if err := newStore(home).Open(); err != nil {
		t.Fatal(err)
	}
	if again := ParseRecords(read(t, home, RecordsFile), t0); again.Recs[1].ID != first.Recs[1].ID {
		t.Errorf("ids changed between boots: %s, %s", first.Recs[1].ID, again.Recs[1].ID)
	}
}

// Two processes (serve, a oneshot) on one home: each read, change and write happens under the
// home's lock, so neither writes over what the other just saved.
func TestTwoStoresLoseNothing(t *testing.T) {
	home := newHome(t, "", "")
	a, b := &Store{Home: home}, &Store{Home: home}
	var wg sync.WaitGroup
	for i, s := range []*Store{a, b, a, b} {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			for j := 0; j < 15; j++ {
				if _, err := s.Save(SaveOptions{Text: fmt.Sprintf("Store %d note %d about topic%d%d.", i, j, i, j), Force: true}); err != nil {
					t.Error(err)
				}
			}
		}(i, s)
	}
	wg.Wait()
	if p := ParseRecords(read(t, home, RecordsFile), t0); len(p.Recs) != 60 {
		t.Errorf("%d records of 60", len(p.Recs))
	}
}

// A change another process made is seen even when the file kept its size and its mtime.
func TestFingerprintSeesASameSizeRewrite(t *testing.T) {
	home := newHome(t, "Alpha.", "")
	s := newStore(home)
	if got, _ := s.Entries(TargetMemory); !slices.Equal(got, []string{"Alpha."}) {
		t.Fatal(got)
	}
	p := filepath.Join(home, "memories", "MEMORY.md")
	info, _ := os.Stat(p)
	tmp := p + ".new"
	_ = os.WriteFile(tmp, []byte("Gamma."), 0o640)
	_ = os.Chtimes(tmp, info.ModTime(), info.ModTime())
	_ = os.Rename(tmp, p)
	if got, _ := s.Entries(TargetMemory); !slices.Equal(got, []string{"Gamma."}) {
		t.Errorf("entries = %q (a stale read)", got)
	}
}

// failWrite makes the n-th file a change writes fail (a crash between the writes).
func failWrite(t *testing.T, n int) {
	t.Helper()
	count := 0
	beforeWrite = func(string) error {
		if count++; count == n {
			return errors.New("crash")
		}
		return nil
	}
	t.Cleanup(func() { beforeWrite = nil })
}

// A removal that stops between its two writes leaves the entry archived, never back in the hot
// file: the hot file is written first.
func TestCrashedRemovalArchives(t *testing.T) {
	for _, op := range []string{"remove", "delete", "unpin"} {
		home := newHome(t, "A.\n§\nB.", "")
		s := newStore(home)
		id := ""
		res, _ := s.Search(Query{Scope: "hot"})
		for _, v := range res.Results {
			if v.Text == "B." {
				id = v.ID
			}
		}
		failWrite(t, 2)
		var err error
		switch op {
		case "remove":
			_, err = s.Remove(TargetMemory, "B.")
		case "delete":
			err = s.Delete(Audience{}, id, false)
		case "unpin":
			_, _, err = s.Unpin(Audience{}, id)
		}
		if err == nil {
			t.Fatalf("%s: the second write did not fail", op)
		}
		beforeWrite = nil
		if got, _ := newStore(home).Entries(TargetMemory); !slices.Equal(got, []string{"A."}) {
			t.Errorf("%s: entries %q: resurrected", op, got)
		}
	}
}

// A nap that caught an older hot file with a newer RECORDS.md (or a crash after RECORDS.md was
// written) does not bring a deleted entry back: its tombstone in RECORDS.md drops it from the hot
// file. Saving the text again clears the tombstone.
func TestTombstonesStopResurrection(t *testing.T) {
	home := newHome(t, "A.\n§\nB.\n§\nC.", "Ana likes tea.")
	s := newStore(home)
	if _, err := s.Remove(TargetMemory, "B."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Replace(TargetMemory, "C.", "C, corrected."); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove(TargetUser, "Ana"); err != nil {
		t.Fatal(err)
	}
	// the nap's older hot files come back with the newer archive
	_ = os.WriteFile(filepath.Join(home, "memories", "MEMORY.md"), []byte("A.\n§\nB.\n§\nC."), 0o640)
	_ = os.WriteFile(filepath.Join(home, "memories", "USER.md"), []byte("Ana likes tea."), 0o640)
	s2 := newStore(home)
	if got, _ := s2.Entries(TargetMemory); !slices.Equal(got, []string{"A."}) {
		t.Errorf("memory entries %q", got)
	}
	if got, _ := s2.Entries(TargetUser); len(got) != 0 {
		t.Errorf("user entries %q", got)
	}
	if f := read(t, home, "memories/MEMORY.md"); f != "A." {
		t.Errorf("MEMORY.md = %q", f)
	}
	for _, r := range ParseRecords(read(t, home, RecordsFile), t0).Recs {
		if r.Text == "B." || r.Text == "Ana likes tea." {
			t.Errorf("resurrected: %+v", r)
		}
	}
	if !strings.Contains(read(t, home, RecordsFile), "needletail-tombstones") || strings.Contains(read(t, home, RecordsFile), "B.") {
		t.Error("no tombstone, or the deleted text in clear")
	}
	// saved again on purpose: no tombstone stops it
	if _, err := s2.Add(TargetMemory, "B."); err != nil {
		t.Fatal(err)
	}
	if p := ParseRecords(read(t, home, RecordsFile), t0); len(p.Tombs) != 2 {
		t.Errorf("tombstones %+v", p.Tombs)
	}
	_ = os.WriteFile(filepath.Join(home, "memories", "MEMORY.md"), []byte("A.\n§\nB."), 0o640)
	if got, _ := newStore(home).Entries(TargetMemory); !slices.Equal(got, []string{"A.", "B."}) {
		t.Errorf("re-added entry dropped: %q", got)
	}
}

// Tombstones are bounded: at most MaxTombstones (the newest kept), none older than TombstoneAge.
func TestTombstonesBounded(t *testing.T) {
	tombs := map[string]string{}
	for i := 0; i < MaxTombstones+20; i++ {
		tombs[fmt.Sprintf("memory\x00h%03d", i)] = stamp(t0.Add(time.Duration(i) * time.Second))
	}
	pruneTombs(tombs, t0.Add(time.Hour))
	if len(tombs) != MaxTombstones || tombs["memory\x00h000"] != "" || tombs[fmt.Sprintf("memory\x00h%03d", MaxTombstones+19)] == "" {
		t.Errorf("%d tombstones, the oldest kept or the newest dropped", len(tombs))
	}
	pruneTombs(tombs, t0.Add(TombstoneAge+time.Hour))
	if len(tombs) != 0 {
		t.Errorf("%d tombstones after they aged", len(tombs))
	}
	// through the store: an aged tombstone is gone at the next write
	home := newHome(t, "", "")
	now := t0
	s := &Store{Home: home, Now: func() time.Time { return now }}
	for i := 0; i < 2; i++ {
		r, _ := s.Save(SaveOptions{Text: fmt.Sprintf("Record number %d.", i), Force: true})
		if err := s.Delete(Audience{}, r.ID, false); err != nil {
			t.Fatal(err)
		}
	}
	if p := ParseRecords(read(t, home, RecordsFile), t0); len(p.Tombs) != 2 {
		t.Errorf("%d tombstones", len(p.Tombs))
	}
	now = now.Add(TombstoneAge + time.Hour)
	r, _ := s.Save(SaveOptions{Text: "One more.", Force: true})
	_ = s.Delete(Audience{}, r.ID, false)
	if p := ParseRecords(read(t, home, RecordsFile), t0); len(p.Tombs) != 1 {
		t.Errorf("%d tombstones after they aged", len(p.Tombs))
	}
}
