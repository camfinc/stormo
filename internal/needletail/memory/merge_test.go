package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func fullHash(text string) string {
	sum := sha256.Sum256([]byte(Normalize(text)))
	return hex.EncodeToString(sum[:])
}

// rehydrateTo stands for the merge at boot: it rewrites the hot files and leaves the report.
func rehydrateTo(t *testing.T, home, memory, user, report string) {
	t.Helper()
	future := time.Now().Add(time.Minute)
	for rel, body := range map[string]string{"memories/MEMORY.md": memory, "memories/USER.md": user} {
		p := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, future, future)
	}
	if report != "" {
		p := filepath.Join(home, filepath.FromSlash(MergeFile))
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(report), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The merge report (rehydrate's .swarm/memory-merge.json): a hot entry the merge purged (a
// reviewer rejected it) leaves the archive too, tombstoned, every copy of it; one it deferred (no
// room) stays, archived; one only a nap skew took out of its hot file stays, archived. The report
// is consumed.
func TestMergeReportPurgesRejected(t *testing.T) {
	home := newHome(t, "Kept.\n§\nRejected lesson.\n§\nNo room for it.\n§\nSkewed.", "Ana wants EUR.")
	rec := func(id, kind string, hot int, text string) string {
		return Delimiter + `<!-- {"id":"` + id + `","kind":"` + kind + `","tags":[],"imp":3,"hot":` + string(rune('0'+hot)) +
			`,"src":"chat:s","created":"2026-10-01T00:00:00Z","updated":"2026-10-01T00:00:00Z","scope":"agent"} -->` + "\n" + text
	}
	body := versionLine + rec("m00000000001", KindFact, 1, "Kept.") + rec("m00000000002", KindFact, 2, "Rejected lesson.") +
		rec("m00000000003", KindFact, 3, "No room for it.") + rec("m00000000004", KindFact, 4, "Skewed.") +
		rec("m00000000005", KindEpisode, 0, "rejected   LESSON.") + // an archived copy of the rejected text: it goes too
		rec("m00000000006", KindUser, 1, "Ana wants EUR.")
	_ = os.WriteFile(filepath.Join(home, filepath.FromSlash(RecordsFile)), []byte(body), 0o600)
	var logs []string
	s := newStore(home)
	s.Log = func(l string) { logs = append(logs, l) }
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	if n := len(ParseRecords(read(t, home, RecordsFile), t0).Recs); n != 6 {
		t.Fatalf("%d records before the merge", n)
	}
	report := `{"v":1,"memory":{"purged":["` + fullHash("Rejected lesson.") + `"],"deferred":["` + fullHash("No room for it.") + `","` + fullHash("A seed that did not fit.") + `"]},` +
		`"user":{"purged":["` + fullHash("Ana wants EUR.") + `"],"deferred":[]}}`
	rehydrateTo(t, home, "Kept.", "", report)
	logs = nil
	got, _ := s.Entries(TargetMemory)
	if !slices.Equal(got, []string{"Kept."}) {
		t.Errorf("entries = %q", got)
	}
	p := ParseRecords(read(t, home, RecordsFile), t0)
	hot := map[string]int{}
	for _, r := range p.Recs {
		hot[r.Text] = r.Hot
	}
	if len(p.Recs) != 3 || hot["Kept."] != 1 || hot["No room for it."] != 0 || hot["Skewed."] != 0 {
		t.Errorf("records = %v", hot)
	}
	for _, text := range []string{"Rejected lesson.", "Ana wants EUR."} {
		if strings.Contains(read(t, home, RecordsFile), strings.TrimSuffix(text, ".")) {
			t.Errorf("%q is still in the archive", text)
		}
	}
	tombs := map[string]bool{}
	for _, tb := range p.Tombs {
		tombs[tb.Target+":"+tb.Hash] = true
	}
	if !tombs["memory:"+TextHash("Rejected lesson.")] || !tombs["user:"+TextHash("Ana wants EUR.")] || len(tombs) != 2 {
		t.Errorf("tombstones = %v", p.Tombs)
	}
	if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(MergeFile))); !os.IsNotExist(err) {
		t.Errorf("the report was not consumed: %v", err)
	}
	if l := strings.Join(logs, "\n"); !strings.Contains(l, "3 records a reviewer rejected deleted, 1 deferred for room archived") || !strings.Contains(l, "1 not in their hot file moved to the archive") {
		t.Errorf("logs:\n%s", l)
	}
	// saving the text again, on purpose, works
	if r, err := s.Save(SaveOptions{Text: "Rejected lesson.", Force: true}); err != nil || r.Status != StatusCreated {
		t.Errorf("save again: %+v %v", r, err)
	}
}

// A report this runtime cannot read (a newer format, or not JSON) changes nothing and is kept; with
// no report, a hot record its hot file lost is archived, as before.
func TestMergeReportUnreadable(t *testing.T) {
	for _, report := range []string{`{"v":2,"memory":{"purged":["` + fullHash("B.") + `"]}}`, `not json`} {
		home := newHome(t, "A.\n§\nB.", "")
		s := newStore(home)
		if err := s.Open(); err != nil {
			t.Fatal(err)
		}
		rehydrateTo(t, home, "A.", "", report)
		if got, _ := s.Entries(TargetMemory); !slices.Equal(got, []string{"A."}) {
			t.Errorf("entries = %q", got)
		}
		p := ParseRecords(read(t, home, RecordsFile), t0)
		if len(p.Recs) != 2 || len(p.Tombs) != 0 {
			t.Errorf("%s: records %d, tombstones %v", report, len(p.Recs), p.Tombs)
		}
		if read(t, home, MergeFile) != report {
			t.Errorf("%s: the report changed", report)
		}
	}
}
