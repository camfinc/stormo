package memory

import (
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Benchmarks at the design point (docs/needletail.md §4.6): 10k records of 25-75 words drawn from
// a Zipf vocabulary of 30k words. go test -run '^$' -bench . -benchmem ./internal/needletail/memory/

const benchRecords = 10000

func benchWord(rank uint64) string { return "w" + strconv.FormatUint(rank, 36) }

func benchCorpus(n int) []*Record {
	rng := rand.New(rand.NewSource(1))
	z := rand.NewZipf(rng, 1.1, 1, 29999)
	out := make([]*Record, n)
	for i := range out {
		words := make([]string, 25+rng.Intn(51))
		for j := range words {
			words[j] = benchWord(z.Uint64())
		}
		id := strconv.FormatInt(int64(i), 36)
		out[i] = &Record{ID: "m" + strings.Repeat("0", 11-len(id)) + id, Kind: KindFact, Tags: []string{benchWord(z.Uint64())}, Imp: 1 + i%5,
			Src: "chat:bench", Created: "2026-10-20T14:02:11Z", Updated: "2026-10-20T14:02:11Z", Text: strings.Join(words, " ") + "."}
	}
	return out
}

// benchQueries are 200 queries of 3-5 words: mixed draws from the whole vocabulary (often one
// very common word), selective leaves out the 100 most common (what stop words leave).
func benchQueries(selective bool) []string {
	rng := rand.New(rand.NewSource(2))
	z := rand.NewZipf(rng, 1.1, 1, 29999)
	out := make([]string, 200)
	for i := range out {
		words := make([]string, 3+rng.Intn(3))
		for j := range words {
			r := z.Uint64()
			for selective && r < 100 {
				r = z.Uint64()
			}
			words[j] = benchWord(r)
		}
		out[i] = strings.Join(words, " ")
	}
	return out
}

func benchStore(b *testing.B, recs []*Record) *Store {
	b.Helper()
	home := b.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "memories"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, filepath.FromSlash(RecordsFile)), []byte(FormatRecords(recs, nil)), 0o644); err != nil {
		b.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	s := &Store{Home: home, Now: func() time.Time { return now }}
	if err := s.Open(); err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkIndexBuild(b *testing.B) {
	recs := benchCorpus(benchRecords)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	keep := newIndex()
	for _, r := range recs {
		keep.add(r)
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		x := newIndex()
		for _, r := range recs {
			x.add(r)
		}
	}
	b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), "index-heap-MB")
	runtime.KeepAlive(keep)
}

func BenchmarkParseRecords(b *testing.B) {
	raw := FormatRecords(benchCorpus(benchRecords), nil)
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if p := ParseRecords(raw, time.Now()); len(p.Recs) != benchRecords {
			b.Fatal(len(p.Recs))
		}
	}
}

// BenchmarkOpen is a boot: read RECORDS.md, reconcile, build the index.
func BenchmarkOpen(b *testing.B) {
	s := benchStore(b, benchCorpus(benchRecords))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.st = nil
		if err := s.Open(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchSearch(b *testing.B, selective bool) {
	s := benchStore(b, benchCorpus(benchRecords))
	qs := benchQueries(selective)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Search(Query{Text: qs[i%len(qs)]}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchMixed(b *testing.B)     { benchSearch(b, false) }
func BenchmarkSearchSelective(b *testing.B) { benchSearch(b, true) }

func BenchmarkRecall(b *testing.B) {
	s := benchStore(b, benchCorpus(benchRecords))
	qs := benchQueries(true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Recall("Tell me about "+qs[i%len(qs)], nil, 0, 0)
	}
}

// BenchmarkSave is one write: a dedupe check, then RECORDS.md rewritten (atomically, fsynced).
func BenchmarkSave(b *testing.B) {
	s := benchStore(b, benchCorpus(benchRecords))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Save(SaveOptions{Text: "Benchmark note number " + strconv.Itoa(i) + " about zzbench" + strconv.Itoa(i), Force: true}); err != nil {
			b.Fatal(err)
		}
	}
}

// benchNear is n texts each a corpus record's with its last word left out: a save of one finds
// that record similar (nothing written), so the benchmark is the dedupe check alone.
func benchNear(recs []*Record, n int) []string {
	out := make([]string, n)
	for i := range out {
		words := strings.Fields(strings.TrimSuffix(recs[(i*37)%len(recs)].Text, "."))
		out[i] = strings.Join(words[:len(words)-1], " ")
	}
	return out
}

// BenchmarkSaveNonForce is a save without force: the similar-records check over every BM25 hit.
func BenchmarkSaveNonForce(b *testing.B) {
	recs := benchCorpus(benchRecords)
	s := benchStore(b, recs)
	texts := benchNear(recs, 200)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := s.Save(SaveOptions{Text: texts[i%len(texts)]})
		if err != nil || res.Status != StatusSimilar {
			b.Fatal(res.Status, err)
		}
	}
}

// BenchmarkSearchPhrase is a search with "a phrase" of two common words (most records hit a word).
func BenchmarkSearchPhrase(b *testing.B) {
	recs := benchCorpus(benchRecords)
	s := benchStore(b, recs)
	qs := make([]string, 200)
	for i := range qs {
		words := strings.Fields(recs[(i*53)%len(recs)].Text)
		qs[i] = `"` + words[0] + " " + words[1] + `"`
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r, err := s.Search(Query{Text: qs[i%len(qs)]}); err != nil || r.Total == 0 {
			b.Fatal(r.Total, err)
		}
	}
}
