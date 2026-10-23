package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// The merge report (rehydrate's .swarm/memory-merge.json), as an older rehydrate wrote it (v1: bare
// hashes, no times): a hot entry the merge purged (a reviewer rejected it) leaves the archive too, tombstoned, every copy of it; one it deferred (no
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
	if l := strings.Join(logs, "\n"); !strings.Contains(l, "3 records a reviewer rejected deleted, 0 saved again since the rejection archived, 1 deferred for room archived") || !strings.Contains(l, "1 not in their hot file moved to the archive") {
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
	for _, report := range []string{`{"v":3,"memory":{"purged":[{"hash":"` + fullHash("B.") + `"}]}}`, `not json`} {
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

// v2: each rejection carries when it was made. Every copy saved before it goes (hot, archived, in
// the second of the rejection, or at any time when the time is unknown), tombstoned; one saved
// again after it stays, archived (rehydrate keeps its text out of the hot file), and its text is
// not tombstoned. Applying the same report again (rehydrate writes it whole on every boot, and a
// nap can catch the home before the runtime read it) changes nothing, however long the list.
func TestMergeReportRejectedAt(t *testing.T) {
	home := newHome(t, "Kept.\n§\nold rejected.", "Kept user.")
	rec := func(id, kind string, hot int, created, text string) string {
		return Delimiter + `<!-- {"id":"` + id + `","kind":"` + kind + `","tags":[],"imp":3,"hot":` + string(rune('0'+hot)) +
			`,"src":"chat:s","created":"` + created + `","updated":"` + created + `","scope":"agent"} -->` + "\n" + text
	}
	body := versionLine + rec("m00000000001", KindFact, 1, "2026-10-01T00:00:00Z", "Kept.") +
		rec("m00000000002", KindFact, 0, "2026-10-01T00:00:00Z", "Old rejected.") +
		rec("m00000000003", KindEpisode, 0, "2026-10-02T00:00:00Z", "old   REJECTED.") +
		rec("m00000000004", KindFact, 2, "2026-10-10T00:00:00Z", "old rejected.") + // saved again after the rejection, and pinned
		rec("m00000000005", KindFact, 0, "2026-10-15T00:00:00Z", "No time.") +
		rec("m00000000006", KindFact, 0, "2026-10-05T12:00:00Z", "Same second.") +
		rec("m00000000007", KindFact, 0, "not a time", "Unreadable created.") +
		rec("m00000000008", KindUser, 1, "2026-10-01T00:00:00Z", "Kept user.")
	_ = os.WriteFile(filepath.Join(home, filepath.FromSlash(RecordsFile)), []byte(body), 0o600)
	var logs []string
	s := newStore(home)
	s.Log = func(l string) { logs = append(logs, l) }
	if err := s.Open(); err != nil {
		t.Fatal(err)
	}
	purged := []string{
		`{"hash":"` + fullHash("Old rejected.") + `","at":"2026-10-05T00:00:00.000Z"}`,
		`{"hash":"` + fullHash("No time.") + `"}`,
		`{"hash":"` + fullHash("Same second.") + `","at":"2026-10-05T12:00:00.400Z"}`,
		`{"hash":"` + fullHash("Unreadable created.") + `","at":"2026-10-05T00:00:00.000Z"}`,
	}
	for i := range 100000 { // rejections the nap no longer holds
		purged = append(purged, `{"hash":"`+fullHash(fmt.Sprint("gone ", i))+`","at":"2026-10-05T00:00:00.000Z"}`)
	}
	report := `{"v":2,"memory":{"purged":[` + strings.Join(purged, ",") + `],"deferred":[]},"user":{"purged":[],"deferred":[]}}`
	rehydrateTo(t, home, "Kept.", "Kept user.", report)
	logs = nil
	if got, _ := s.Entries(TargetMemory); !slices.Equal(got, []string{"Kept."}) {
		t.Errorf("entries = %q", got)
	}
	after := read(t, home, RecordsFile)
	p := ParseRecords(after, t0)
	recs := map[string]*Record{}
	for _, r := range p.Recs {
		recs[r.ID] = r
	}
	if len(p.Recs) != 3 || recs["m00000000001"] == nil || recs["m00000000008"] == nil || recs["m00000000004"] == nil || recs["m00000000004"].Hot != 0 {
		t.Errorf("records:\n%s", after)
	}
	tombs := map[string]bool{}
	for _, tb := range p.Tombs {
		tombs[tb.Target+":"+tb.Hash] = true
	}
	if !tombs["memory:"+TextHash("No time.")] || !tombs["memory:"+TextHash("Same second.")] || !tombs["memory:"+TextHash("Unreadable created.")] ||
		tombs["memory:"+TextHash("Old rejected.")] || len(tombs) != 3 {
		t.Errorf("tombstones = %v", p.Tombs)
	}
	if l := strings.Join(logs, "\n"); !strings.Contains(l, "5 records a reviewer rejected deleted, 1 saved again since the rejection archived, 0 deferred") ||
		strings.Contains(l, "moved to the archive (a nap skew") {
		t.Errorf("logs:\n%s", l)
	}
	if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(MergeFile))); !os.IsNotExist(err) {
		t.Errorf("the report was not consumed: %v", err)
	}

	// the next boot writes the same report (from a nap taken before or after this one read it)
	rehydrateTo(t, home, "Kept.", "Kept user.", report)
	logs = nil
	if got, _ := s.Entries(TargetMemory); !slices.Equal(got, []string{"Kept."}) {
		t.Errorf("entries after the replay = %q", got)
	}
	if again := read(t, home, RecordsFile); again != after {
		t.Errorf("the replay changed RECORDS.md:\n%s\nwas:\n%s", again, after)
	}
	if l := strings.Join(logs, "\n"); strings.Contains(l, "merge report applied") {
		t.Errorf("the replay touched records:\n%s", l)
	}
	if _, err := os.Stat(filepath.Join(home, filepath.FromSlash(MergeFile))); !os.IsNotExist(err) {
		t.Errorf("the replayed report was not consumed: %v", err)
	}
}
