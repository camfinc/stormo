// Package history is session_search (docs/needletail.md §4.5): the agent's own past
// conversations (sessions/*.jsonl) searched by keyword, scrolled around a hit, read, or browsed,
// within the audience of the turn that asks.
//
// The index is in memory and derived, never napped: built lazily at the first search and brought
// up to date at each one by reading only what was appended to a session file since (the files
// are append-only JSONL; a header rewrite at a session's end is noticed and keeps what was read).
// One document per session (BM25 over its people's and the agent's words; the archive's
// tokenizer, memory.Tokens), so its size grows with the sessions' distinct words, not their
// rows, and is bounded (MaxPostings): past it, the least recently written sessions leave the
// index and a search says from when it covers. Only the best candidate sessions are then read
// again, to pick the message that matched, its snippet and the messages around it.
package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// DefaultMaxPostings bounds the index (8 bytes each, plus the terms: about 30 MiB at the bound),
// some 5,000 to 10,000 sessions of ordinary chat and job runs.
const DefaultMaxPostings = 2 << 20

// BM25's constants, as the memory archive's.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// maxLine is the longest message line read (as the store reads them).
const maxLine = 64 << 20

// Index is the sessions directory's keyword index. Safe for concurrent use.
type Index struct {
	// Dir is the sessions directory.
	Dir string
	// MaxPostings bounds the index (0: DefaultMaxPostings).
	MaxPostings int
	Log         func(string)

	mu      sync.Mutex
	docs    []*doc
	byID    map[string]int32
	post    map[string][]posting
	live    int     // live documents
	total   float64 // their lengths
	npost   int     // live postings
	garbage int     // postings of documents no longer live
	sorted  []string
	stale   bool
	// floor: files last written before it are not indexed (the bound evicted them)
	floor time.Time
	// heads are every listed file's header, indexed or not: a conversation's root and source
	// (whose audience decides who sees it) never depend on what the bound kept
	heads map[string]*head
}

// head is what is known of every session file from its header.
type head struct {
	parent, source, created, title string
	mtime                          time.Time
}

// posting is one document's frequency of a term in its people's (user) and the agent's
// (assistant) messages. A document read in several steps has a posting per step.
type posting struct {
	doc        int32
	user, asst uint16
}

// doc is one session file.
type doc struct {
	wire.Session
	body    int64  // bytes after the header line read so far (whole lines only)
	rows    int    // message rows read (the next row's id)
	carryAt string // a compaction's summary time: rows before it are carried copies
	// carriedRows is how many rows it carried over from its parent: the parent's last ones, which
	// are in this session too (and in the context of a turn in it)
	carriedRows int
	mtime       time.Time // the file's, when last read
	size        int64
	length      float64 // terms indexed
	npost       int
	lastAt      string
	visible     int // rows a transcript shows (not system)
	preview     string
	dead        bool
}

func (x *Index) logf(format string, args ...any) {
	if x.Log != nil {
		x.Log(strings.TrimSpace(sprintf(format, args...)))
	}
}

func (x *Index) max() int {
	if x.MaxPostings > 0 {
		return x.MaxPostings
	}
	return DefaultMaxPostings
}

// postings is how many postings the index holds (live and not yet collected).
func (x *Index) postings() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.npost + x.garbage
}

// refresh brings the index up to date with the directory; x.mu is held. Files are visited most
// recently written first, so a first build that reaches the bound keeps the newest.
func (x *Index) refresh() {
	if x.byID == nil {
		x.byID, x.post, x.heads = map[string]int32{}, map[string][]posting{}, map[string]*head{}
	}
	entries, err := os.ReadDir(x.Dir)
	if err != nil {
		x.logf("session search: %v", err)
		return
	}
	type file struct {
		id   string
		info os.FileInfo
	}
	var files []file
	seen := map[string]bool{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !sessions.ValidID(id) || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if h := x.heads[id]; h == nil || !h.mtime.Equal(info.ModTime()) {
			s, ok := readHeader(filepath.Join(x.Dir, e.Name()), id)
			if !ok {
				continue // no whole header yet
			}
			x.heads[id] = &head{parent: s.Parent, source: s.Source, created: s.Created, title: s.Title, mtime: info.ModTime()}
		}
		seen[id] = true
		files = append(files, file{id, info})
	}
	for id, n := range x.byID {
		if !seen[id] {
			x.drop(n)
			delete(x.byID, id)
		}
	}
	for id := range x.heads {
		if !seen[id] {
			delete(x.heads, id)
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].info.ModTime().After(files[j].info.ModTime()) })
	for _, f := range files {
		n, known := x.byID[f.id]
		if known {
			d := x.docs[n]
			if d.dead {
				delete(x.byID, f.id) // evicted: read again only if written since
				known = false
			} else if d.size == f.info.Size() && d.mtime.Equal(f.info.ModTime()) {
				continue
			}
		}
		if !known && f.info.ModTime().Before(x.floor) {
			continue
		}
		x.read(f.id, f.info)
		for x.npost > x.max() && x.evictOldest() {
		}
	}
	if x.garbage > 0 && (x.npost+x.garbage > x.max() || x.garbage > x.npost/4) {
		x.collect()
	}
}

// readHeader is a session file's header.
func readHeader(path, id string) (wire.Session, bool) {
	f, err := os.Open(path)
	if err != nil {
		return wire.Session{}, false
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(f, 4096).ReadSlice('\n')
	var h wire.Session
	if err != nil || json.Unmarshal(bytes.TrimSpace(line), &h) != nil || h.V != wire.SessionVersion || h.ID != id {
		return wire.Session{}, false
	}
	return h, true
}

// read indexes what was appended to id's file since the last read (all of it for a new one).
func (x *Index) read(id string, info os.FileInfo) {
	f, err := os.Open(filepath.Join(x.Dir, id+".jsonl"))
	if err != nil {
		return
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	line, err := br.ReadSlice('\n')
	if err != nil {
		return // no whole header yet
	}
	headerLen := int64(len(line))
	var h wire.Session
	if json.Unmarshal(bytes.TrimSpace(line), &h) != nil || h.V != wire.SessionVersion || h.ID != id {
		return
	}
	var d *doc
	if n, ok := x.byID[id]; ok {
		d = x.docs[n]
		if d.Created != h.Created || info.Size()-headerLen < d.body {
			x.drop(n) // another file under the same id: read afresh
			delete(x.byID, id)
			d = nil
		}
	}
	if d == nil {
		d = &doc{}
		x.byID[id] = int32(len(x.docs))
		x.docs = append(x.docs, d)
		x.live++
	}
	d.Session, d.size, d.mtime = h, info.Size(), info.ModTime()
	if d.body > 0 {
		if _, err := f.Seek(headerLen+d.body, 0); err != nil {
			return
		}
		br.Reset(f)
	}
	tf := map[string]*posting{}
	var length float64
	for {
		line, err := readLine(br)
		if err != nil {
			break // EOF, or a line not whole yet: read next time
		}
		d.body += int64(len(line))
		var m wire.Message
		if t := bytes.TrimSpace(line); len(t) == 0 || json.Unmarshal(t, &m) != nil {
			continue
		}
		d.rows++
		if m.At != "" {
			d.lastAt = m.At
		}
		if m.Role == "system" {
			if d.Parent != "" && d.carryAt == "" {
				d.carryAt = m.At
			}
			continue
		}
		d.visible++
		if d.preview == "" && m.Role == "user" && m.Kind == "" {
			d.preview = preview(cleanUser(m.Content).text)
		}
		if carried(d, m) {
			d.carriedRows++
			continue
		}
		text, ok := indexText(m)
		if !ok {
			continue
		}
		for _, t := range memory.Tokens(text) {
			p := tf[t]
			if p == nil {
				p = &posting{}
				tf[t] = p
			}
			if m.Role == "user" {
				p.user = sat(p.user)
			} else {
				p.asst = sat(p.asst)
			}
			length++
		}
	}
	n := x.byID[id]
	for t, p := range tf {
		if _, ok := x.post[t]; !ok {
			x.stale = true
		}
		p.doc = n
		x.post[t] = append(x.post[t], *p)
	}
	d.npost += len(tf)
	x.npost += len(tf)
	d.length += length
	x.total += length
}

// readLine is the next whole line (with its newline); a last line without one is not whole.
func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(out)+len(chunk) > maxLine {
			return nil, bufio.ErrTooLong
		}
		out = append(out, chunk...)
		switch err {
		case nil:
			return out, nil
		case bufio.ErrBufferFull:
			continue
		default:
			return nil, err
		}
	}
}

func sat(n uint16) uint16 {
	if n == math.MaxUint16 {
		return n
	}
	return n + 1
}

// carried: m is a copy of the parent's row a compaction carried over (verbatim, its time
// before the summary's), already indexed in the parent.
func carried(d *doc, m wire.Message) bool {
	return d.Parent != "" && d.carryAt != "" && m.At != "" && m.At < d.carryAt
}

// drop takes a document out of the index (deleted, replaced, evicted); its postings are garbage
// until collect.
func (x *Index) drop(n int32) {
	d := x.docs[n]
	if d.dead {
		return
	}
	d.dead = true
	x.live--
	x.total -= d.length
	x.npost -= d.npost
	x.garbage += d.npost
}

// evictOldest drops the least recently written live document and raises the floor past it, so
// files older than it are not read again; false when there is none.
func (x *Index) evictOldest() bool {
	var oldest int32 = -1
	for id, n := range x.byID {
		d := x.docs[n]
		if d.dead {
			continue
		}
		if oldest < 0 || d.mtime.Before(x.docs[oldest].mtime) || d.mtime.Equal(x.docs[oldest].mtime) && id < x.docs[oldest].ID {
			oldest = n
		}
	}
	if oldest < 0 {
		return false
	}
	d := x.docs[oldest]
	x.drop(oldest)
	if f := d.mtime.Add(time.Nanosecond); f.After(x.floor) {
		x.floor = f
	}
	if x.npost+x.garbage > x.max() {
		x.collect()
	}
	return true
}

// collect removes the postings of documents no longer live.
func (x *Index) collect() {
	for t, ps := range x.post {
		keep := ps[:0]
		for _, p := range ps {
			if !x.docs[p.doc].dead {
				keep = append(keep, p)
			}
		}
		if len(keep) == 0 {
			delete(x.post, t)
			x.stale = true
		} else {
			x.post[t] = keep
		}
	}
	x.garbage = 0
}

// expand is the index terms a query term stands for: itself, or with a trailing * every term it
// prefixes (at most 64, the most frequent first).
func (x *Index) expand(term string) []string {
	prefix, ok := strings.CutSuffix(term, "*")
	if !ok {
		return []string{term}
	}
	if len([]rune(prefix)) < 2 {
		return nil
	}
	if x.stale || x.sorted == nil {
		x.sorted = make([]string, 0, len(x.post))
		for t := range x.post {
			x.sorted = append(x.sorted, t)
		}
		sort.Strings(x.sorted)
		x.stale = false
	}
	out := []string{}
	for i := sort.SearchStrings(x.sorted, prefix); i < len(x.sorted) && strings.HasPrefix(x.sorted[i], prefix); i++ {
		out = append(out, x.sorted[i])
	}
	if len(out) > 64 {
		sort.SliceStable(out, func(i, j int) bool { return len(x.post[out[i]]) > len(x.post[out[j]]) })
		out = out[:64]
	}
	return out
}

// score is every live document holding one of terms in the roles asked (user, asst), with its
// BM25 score; idf is each term's, for the rows' own ranking.
func (x *Index) score(terms []string, user, asst bool) (map[int32]float64, map[string]float64) {
	scores := map[int32]float64{}
	idf := map[string]float64{}
	if x.live == 0 {
		return scores, idf
	}
	avg := x.total / float64(x.live)
	if avg == 0 {
		avg = 1
	}
	tf := map[int32]float64{}
	for _, q := range terms {
		for _, t := range x.expand(q) {
			clear(tf)
			for _, p := range x.post[t] {
				if x.docs[p.doc].dead {
					continue
				}
				var n float64
				if user {
					n += float64(p.user)
				}
				if asst {
					n += float64(p.asst)
				}
				if n > 0 {
					tf[p.doc] += n
				}
			}
			df := float64(len(tf))
			if df == 0 {
				continue
			}
			w := math.Log(1 + (float64(x.live)-df+0.5)/(df+0.5))
			idf[t] = w
			for n, f := range tf {
				l := x.docs[n].length
				scores[n] += w * f * (bm25K1 + 1) / (f + bm25K1*(1-bm25B+bm25B*l/avg))
			}
		}
	}
	return scores, idf
}

// root is a session's conversation (its first session), by the parents the headers name
// (every file's, indexed or not).
func (x *Index) root(id string) string {
	for range 10000 {
		h, ok := x.heads[id]
		if !ok || h.parent == "" {
			return id
		}
		if _, ok := x.heads[h.parent]; !ok {
			return id
		}
		id = h.parent
	}
	return id
}

// source is a conversation's source (its root's; a compaction's child copies it).
func (x *Index) source(id string) string {
	if h, ok := x.heads[x.root(id)]; ok {
		return h.source
	}
	if h, ok := x.heads[id]; ok {
		return h.source
	}
	return ""
}
