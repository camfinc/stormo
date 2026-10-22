package memory

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
