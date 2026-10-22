package memory

import (
	"math"
	"sort"
	"strings"
)

// The keyword index (docs/needletail.md §4.6): BM25 over the records' text and tags, in memory,
// built at boot and changed in place on every write; derived, never napped.

// BM25's constants (FTS5's) and the tags' weight against the text's.
const (
	bm25K1     = 1.2
	bm25B      = 0.75
	tagsWeight = 2
)

type posting struct {
	doc int32
	tf  float32
}

type doc struct {
	rec    *Record
	length float64
	terms  map[string]float32 // weighted term frequencies
	folded string             // the text's words, folded, space-joined (phrases)
}

type index struct {
	docs  []*doc // by doc number; nil when freed
	free  []int32
	byID  map[string]int32
	post  map[string][]posting
	total float64 // sum of lengths
	n     int
	// sorted is the terms in order (prefix queries), rebuilt when stale
	sorted []string
	stale  bool
}

func newIndex() *index {
	return &index{byID: map[string]int32{}, post: map[string][]posting{}}
}

func (x *index) add(r *Record) {
	if _, ok := x.byID[r.ID]; ok {
		x.remove(r.ID)
	}
	d := &doc{rec: r, terms: map[string]float32{}}
	text := tokens(r.Text)
	for _, t := range text {
		d.terms[t]++
	}
	d.length = float64(len(text))
	for _, tag := range r.Tags {
		for _, t := range tokens(tag) {
			d.terms[t] += tagsWeight
			d.length += tagsWeight
		}
	}
	d.folded = " " + strings.Join(words(r.Text), " ") + " "
	var n int32
	if k := len(x.free); k > 0 {
		n, x.free = x.free[k-1], x.free[:k-1]
		x.docs[n] = d
	} else {
		n = int32(len(x.docs))
		x.docs = append(x.docs, d)
	}
	x.byID[r.ID] = n
	for t, tf := range d.terms {
		if _, ok := x.post[t]; !ok {
			x.stale = true
		}
		x.post[t] = append(x.post[t], posting{n, tf})
	}
	x.total += d.length
	x.n++
}

func (x *index) remove(id string) {
	n, ok := x.byID[id]
	if !ok {
		return
	}
	d := x.docs[n]
	for t := range d.terms {
		ps := x.post[t]
		for i, p := range ps {
			if p.doc == n {
				ps[i] = ps[len(ps)-1]
				ps = ps[:len(ps)-1]
				break
			}
		}
		if len(ps) == 0 {
			delete(x.post, t)
			x.stale = true
		} else {
			x.post[t] = ps
		}
	}
	x.total -= d.length
	x.n--
	x.docs[n] = nil
	x.free = append(x.free, n)
	delete(x.byID, id)
}

// expand is the index terms a query term stands for: itself, or with a trailing * every term it
// prefixes (at most 64, the most frequent first).
func (x *index) expand(term string) []string {
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

func (x *index) idf(term string) float64 {
	df := float64(len(x.post[term]))
	return math.Log(1 + (float64(x.n)-df+0.5)/(df+0.5))
}

// hit is a document a query matched: its BM25 score and which of the query's terms it has.
type hit struct {
	doc     *doc
	score   float64
	matched int // distinct query terms matched
	rare    bool
}

// score is every document that holds at least one of terms (query terms, a trailing * a prefix),
// with its BM25 score. rareDF is the document frequency at or under which a term is rare.
func (x *index) score(terms []string, rareDF int) map[int32]*hit {
	out := map[int32]*hit{}
	if x.n == 0 {
		return out
	}
	avg := x.total / float64(x.n)
	if avg == 0 {
		avg = 1
	}
	for _, q := range terms {
		seen := map[int32]bool{}
		for _, t := range x.expand(q) {
			ps := x.post[t]
			idf := x.idf(t)
			rare := len(ps) <= rareDF
			for _, p := range ps {
				d := x.docs[p.doc]
				tf := float64(p.tf)
				s := idf * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*d.length/avg))
				h := out[p.doc]
				if h == nil {
					h = &hit{doc: d}
					out[p.doc] = h
				}
				h.score += s
				if !seen[p.doc] {
					seen[p.doc] = true
					h.matched++
				}
				h.rare = h.rare || rare
			}
		}
	}
	return out
}
