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
	// terms are its distinct terms (to remove its postings): first its text's, sorted (has), then
	// those only its tags have
	terms []string
	nText int
}

// has: t is a term of the document's text (not only of its tags).
func (d *doc) has(t string) bool {
	i := sort.SearchStrings(d.terms[:d.nText], t)
	return i < d.nText && d.terms[i] == t
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
	tf     map[string]float32 // scratch: a document's weighted term frequencies
}

func newIndex() *index {
	return &index{byID: map[string]int32{}, post: map[string][]posting{}}
}

func (x *index) add(r *Record) {
	if _, ok := x.byID[r.ID]; ok {
		x.remove(r.ID)
	}
	d := &doc{rec: r}
	if x.tf == nil {
		x.tf = map[string]float32{}
	}
	clear(x.tf)
	text := tokens(r.Text)
	for _, t := range text {
		x.tf[t]++
	}
	d.length = float64(len(text))
	var tags []string
	for _, tag := range r.Tags {
		tags = append(tags, tokens(tag)...)
	}
	d.terms = make([]string, 0, len(x.tf)+len(tags))
	for t := range x.tf {
		d.terms = append(d.terms, t)
	}
	sort.Strings(d.terms)
	d.nText = len(d.terms)
	for _, t := range tags {
		if _, ok := x.tf[t]; !ok {
			d.terms = append(d.terms, t)
		}
		x.tf[t] += tagsWeight
		d.length += tagsWeight
	}
	var n int32
	if k := len(x.free); k > 0 {
		n, x.free = x.free[k-1], x.free[:k-1]
		x.docs[n] = d
	} else {
		n = int32(len(x.docs))
		x.docs = append(x.docs, d)
	}
	x.byID[r.ID] = n
	for _, t := range d.terms {
		if _, ok := x.post[t]; !ok {
			x.stale = true
		}
		x.post[t] = append(x.post[t], posting{n, x.tf[t]})
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
	for _, t := range d.terms {
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
// with its BM25 score. rareDF is the document frequency at or under which a term is rare. Scores
// accumulate in dense arrays by document number: no map per query.
func (x *index) score(terms []string, rareDF int) []*hit {
	if x.n == 0 {
		return nil
	}
	avg := x.total / float64(x.n)
	if avg == 0 {
		avg = 1
	}
	at := make([]int32, len(x.docs))   // 1 + the document's index in hits
	last := make([]int32, len(x.docs)) // 1 + the last query term counted for it
	hits := []hit{}
	for qi, q := range terms {
		for _, t := range x.expand(q) {
			ps := x.post[t]
			idf := x.idf(t)
			rare := len(ps) <= rareDF
			for _, p := range ps {
				d := x.docs[p.doc]
				tf := float64(p.tf)
				k := at[p.doc]
				if k == 0 {
					hits = append(hits, hit{doc: d})
					k = int32(len(hits))
					at[p.doc] = k
				}
				h := &hits[k-1]
				h.score += idf * tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*d.length/avg))
				if last[p.doc] != int32(qi+1) {
					last[p.doc] = int32(qi + 1)
					h.matched++
				}
				h.rare = h.rare || rare
			}
		}
	}
	out := make([]*hit, len(hits))
	for i := range hits {
		out[i] = &hits[i]
	}
	return out
}
