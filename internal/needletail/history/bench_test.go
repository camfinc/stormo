package history

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// benchDir writes n synthetic sessions of rows messages, each 10-40 words drawn Zipf-like from a
// 20,000-word vocabulary, in the store's format.
func benchDir(b *testing.B, n, rows int) string {
	b.Helper()
	dir := filepath.Join(b.TempDir(), "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		b.Fatal(err)
	}
	r := rand.New(rand.NewPCG(1, 2))
	z := rand.NewZipf(r, 1.1, 1, 19999)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range n {
		id := fmt.Sprintf("s%05d", i)
		var sb strings.Builder
		h, _ := json.Marshal(wire.Session{V: wire.SessionVersion, ID: id, Source: "api_server", Created: start.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)})
		sb.Write(h)
		sb.WriteByte('\n')
		for j := range rows {
			words := make([]string, 10+r.IntN(30))
			for k := range words {
				words[k] = fmt.Sprintf("w%d", z.Uint64())
			}
			role := "user"
			if j%2 == 1 {
				role = "assistant"
			}
			m, _ := json.Marshal(wire.Message{Role: role, Content: strings.Join(words, " "), At: start.Add(time.Duration(i)*time.Minute + time.Duration(j)*time.Second).Format("2006-01-02T15:04:05.000Z07:00")})
			sb.Write(m)
			sb.WriteByte('\n')
		}
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(sb.String()), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	return dir
}

func benchSearch(b *testing.B, ix *Index, q string) {
	if _, err := ix.Search(Query{Text: q, Limit: 3, Visible: all}); err != nil {
		b.Fatal(err)
	}
}

// The first search builds the index: 5,000 sessions of 20 messages (100,000 messages).
func BenchmarkFirstSearch5k(b *testing.B) {
	dir := benchDir(b, 5000, 20)
	for b.Loop() {
		ix := &Index{Dir: dir}
		benchSearch(b, ix, "w15 w873")
	}
}

// A search on an index that is up to date (each one still lists and stats the directory).
func BenchmarkSearch5k(b *testing.B) {
	dir := benchDir(b, 5000, 20)
	ix := &Index{Dir: dir}
	benchSearch(b, ix, "w15")
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ix2 := &Index{Dir: dir}
	benchSearch(b, ix2, "w15")
	runtime.GC()
	runtime.ReadMemStats(&after)
	for b.Loop() {
		benchSearch(b, ix, "w15 w873 w4211")
	}
	b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), "index-MiB")
	b.ReportMetric(float64(ix2.postings()), "postings")
	runtime.KeepAlive(ix2)
}

// A search after one session grew by a turn: only what was appended is read.
func BenchmarkSearchAfterAppend5k(b *testing.B) {
	dir := benchDir(b, 5000, 20)
	ix := &Index{Dir: dir}
	benchSearch(b, ix, "w15")
	f, err := os.OpenFile(filepath.Join(dir, "s04999.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	for b.Loop() {
		m, _ := json.Marshal(wire.Message{Role: "user", Content: "w1 w2 w3 w99 new words here", At: "2026-02-01T00:00:00.000Z"})
		_, _ = f.Write(append(m, '\n'))
		benchSearch(b, ix, "w99")
	}
}
