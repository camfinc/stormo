package memory

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
)

// Search and recall (docs/needletail.md §4.6): BM25 candidates, re-ranked with small boosts for
// recency, importance and use.

// Ranking weights (keyword, recency, importance, use) and the recency half-life.
const (
	weightLex     = 0.65
	weightRecency = 0.15
	weightImp     = 0.12
	weightUse     = 0.08
	halfLifeDays  = 30
	// candidates is how many BM25 hits are re-ranked.
	candidates = 50
	// searchCut is a search result's longest text (UTF-16 units); ids fetch it whole.
	searchCut = 600
	// DefaultLimit and MaxLimit bound a search's results.
	DefaultLimit = 8
	MaxLimit     = 20
)

// View is a record as search results and answers show it.
type View struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Tags    []string `json:"tags"`
	Hot     int      `json:"hot,omitempty"`
	Imp     int      `json:"imp"`
	Created string   `json:"created"`
	Updated string   `json:"updated"`
	Expires string   `json:"expires,omitempty"`
	Expired bool     `json:"expired,omitempty"`
	PII     []string `json:"pii,omitempty"`
	Scope   string   `json:"scope,omitempty"`
	Src     string   `json:"src,omitempty"`
	Score   float64  `json:"score,omitempty"`
	Text    string   `json:"text"`
	Cut     bool     `json:"cut,omitempty"`
}

func (st *state) view(r *Record, now time.Time, score float64, short bool) View {
	v := View{ID: r.ID, Kind: r.Kind, Tags: append([]string{}, r.Tags...), Hot: r.Hot, Imp: r.Imp, Created: r.Created, Updated: r.Updated,
		Expires: r.Expires, Expired: r.Expired(now), PII: r.PII, Scope: r.Scope, Src: r.Src, Score: round(score), Text: r.Text}
	if short {
		v.Text = cut(r.Text, searchCut)
		v.Cut = v.Text != r.Text
	}
	return v
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

// cut is s within n UTF-16 units, ending with "…" when cut.
func cut(s string, n int) string {
	if Units(s) <= n {
		return s
	}
	used := 0
	for i, r := range s {
		w := len(utf16.Encode([]rune{r}))
		if used+w > n-1 {
			return strings.TrimRight(s[:i], space) + "…"
		}
		used += w
	}
	return s
}

// Query is a search.
type Query struct {
	// Text is plain words (synonyms and both languages help); "a phrase" must appear as it is,
	// word* is a prefix. Empty: the records newest first (with the filters).
	Text string
	// IDs fetches these records whole (a use of each); the other fields are ignored.
	IDs []string
	// Kind, Tags (all must be on the record), Scope (all, hot, archive), Since (updated after:
	// RFC 3339 or YYYY-MM-DD) filter before ranking; expired records only with IncludeExpired.
	Kind           string
	Tags           []string
	Scope          string
	Since          string
	IncludeExpired bool
	// Match: any (default) or all of the words.
	Match string
	Limit int
	// Audience is the turn's (the runtime's): only the records it may see are found or fetched.
	// All finds every scope's: the owner's own runs only (Audience.Operator; ErrAllScopes).
	Audience Audience
	All      bool
}

// SearchResult is a search's answer: Total matches, the best Limit of them.
type SearchResult struct {
	Total   int      `json:"total"`
	Results []View   `json:"results"`
	Missing []string `json:"missing,omitempty"`
}

var phraseRe = regexp.MustCompile(`"([^"]*)"`)

// parseQuery is a query's terms (a trailing * a prefix) and phrases (folded words).
func parseQuery(q string) (terms, phrases []string) {
	seen := map[string]bool{}
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			terms = append(terms, t)
		}
	}
	rest := phraseRe.ReplaceAllStringFunc(q, func(m string) string {
		ws := words(strings.Trim(m, `"`))
		if len(ws) > 0 {
			phrases = append(phrases, strings.Join(ws, " "))
		}
		for _, t := range tokens(strings.Trim(m, `"`)) {
			add(t)
		}
		return " "
	})
	for _, f := range strings.Fields(rest) {
		if p, ok := strings.CutSuffix(f, "*"); ok {
			ws := words(p)
			for i, w := range ws {
				if i == len(ws)-1 {
					add(w + "*")
				} else if !stop[w] {
					add(w)
				}
			}
			continue
		}
		for _, t := range tokens(f) {
			add(t)
		}
	}
	return terms, phrases
}

func parseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("since %q: RFC 3339 or YYYY-MM-DD", s)
}

// Search finds records.
func (s *Store) Search(q Query) (SearchResult, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	if q.All && !q.Audience.Operator {
		return SearchResult{}, ErrAllScopes
	}
	defer s.lock()()
	st, err := s.load()
	if err != nil {
		return SearchResult{}, err
	}
	now := s.now()
	out := SearchResult{Results: []View{}}
	if len(q.IDs) > 0 {
		for _, id := range q.IDs {
			r := st.byID[strings.TrimSpace(id)]
			if r == nil || !s.visible(r, q.Audience, q.All || q.Audience.Operator) {
				out.Missing = append(out.Missing, id)
				continue
			}
			st.use.used(r.ID, now)
			out.Results = append(out.Results, st.view(r, now, 0, false))
		}
		out.Total = len(out.Results)
		s.flushUsage(st, false)
		return out, nil
	}
	match, err := filter(q, now)
	if err != nil {
		return SearchResult{}, err
	}
	keep := func(r *Record) bool { return match(r) && s.visible(r, q.Audience, q.All) }
	terms, phrases := parseQuery(q.Text)
	if strings.TrimSpace(q.Text) == "" {
		recs := []*Record{}
		for _, r := range st.recs {
			if keep(r) {
				recs = append(recs, r)
			}
		}
		sort.SliceStable(recs, func(i, j int) bool {
			if recs[i].Updated != recs[j].Updated {
				return recs[i].Updated > recs[j].Updated
			}
			return recs[i].ID > recs[j].ID
		})
		out.Total = len(recs)
		for _, r := range recs[:min(limit, len(recs))] {
			out.Results = append(out.Results, st.view(r, now, 0, true))
		}
		return out, nil
	}
	if len(terms) == 0 {
		return out, nil
	}
	hits := []*hit{}
	for _, h := range st.idx.score(terms, 0) {
		r := h.doc.rec
		if !keep(r) || q.Match == "all" && h.matched < len(terms) {
			continue
		}
		if len(phrases) > 0 {
			folded := " " + strings.Join(words(r.Text), " ") + " "
			ok := true
			for _, p := range phrases {
				if !strings.Contains(folded, " "+p+" ") {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
		}
		hits = append(hits, h)
	}
	out.Total = len(hits)
	for _, rk := range st.rank(hits, now) {
		if len(out.Results) == limit {
			break
		}
		out.Results = append(out.Results, st.view(rk.r, now, rk.score, true))
	}
	return out, nil
}

// filter is q's filters as a predicate.
func filter(q Query, now time.Time) (func(*Record) bool, error) {
	if q.Kind != "" {
		if _, err := checkKind(q.Kind); err != nil {
			return nil, err
		}
	}
	tags, err := cleanTags(q.Tags)
	if err != nil {
		return nil, err
	}
	switch q.Scope {
	case "", "all", "hot", "archive":
	default:
		return nil, fmt.Errorf("scope %q: all, hot or archive", q.Scope)
	}
	switch q.Match {
	case "", "any", "all":
	default:
		return nil, fmt.Errorf("match %q: any or all", q.Match)
	}
	var since time.Time
	if q.Since != "" {
		if since, err = parseSince(q.Since); err != nil {
			return nil, err
		}
	}
	return func(r *Record) bool {
		if q.Kind != "" && r.Kind != q.Kind || q.Scope == "hot" && r.Hot == 0 || q.Scope == "archive" && r.Hot > 0 {
			return false
		}
		if !q.IncludeExpired && r.Expired(now) {
			return false
		}
		for _, t := range tags {
			if !contains(r.Tags, t) {
				return false
			}
		}
		if !since.IsZero() {
			u, err := time.Parse(time.RFC3339, r.Updated)
			if err != nil || !u.After(since) {
				return false
			}
		}
		return true
	}, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

type ranked struct {
	r     *Record
	lex   float64
	score float64
	h     *hit
}

// rank orders the best candidates BM25 hits by score = 0.65·lex + 0.15·recency + 0.12·importance
// + 0.08·use, lex being BM25 over the best candidate's.
func (st *state) rank(hits []*hit, now time.Time) []ranked {
	hits = top(hits, candidates)
	if len(hits) == 0 {
		return nil
	}
	top := hits[0].score
	out := make([]ranked, len(hits))
	for i, h := range hits {
		lex := h.score / top
		out[i] = ranked{r: h.doc.rec, lex: lex, score: st.boost(h.doc.rec, lex, now), h: h}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].score > out[j].score })
	return out
}

// boost is the ranking formula for a candidate with keyword relevance lex.
func (st *state) boost(r *Record, lex float64, now time.Time) float64 {
	last := r.Updated
	uses := 0
	if u := st.use.recs[r.ID]; u != nil {
		uses = u.Uses
		if u.LastUsed > last {
			last = u.LastUsed
		}
	}
	rec := 0.0
	if t, err := time.Parse(time.RFC3339, last); err == nil {
		days := max(0, now.Sub(t).Hours()/24)
		rec = math.Pow(0.5, days/halfLifeDays)
	}
	imp := float64(r.Imp-1) / 4
	use := min(1, math.Log2(1+float64(uses))/math.Log2(17))
	return weightLex*lex + weightRecency*rec + weightImp*imp + weightUse*use
}

// better orders hits by BM25, then by id (newer first) for a stable order.
func better(a, b *hit) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	return a.doc.rec.ID > b.doc.rec.ID
}

// top is the k best hits in order, selected with a bounded heap (a query's hits can be most of
// the records; only the best k are ranked).
func top(hits []*hit, k int) []*hit {
	if len(hits) <= k {
		sort.Slice(hits, func(i, j int) bool { return better(hits[i], hits[j]) })
		return hits
	}
	h := make([]*hit, 0, k) // a min-heap by better: h[0] is the worst kept
	down := func(i int) {
		for {
			l, w := 2*i+1, i
			if l < len(h) && better(h[w], h[l]) {
				w = l
			}
			if r := l + 1; r < len(h) && better(h[w], h[r]) {
				w = r
			}
			if w == i {
				return
			}
			h[i], h[w] = h[w], h[i]
			i = w
		}
	}
	for _, x := range hits {
		if len(h) < k {
			h = append(h, x)
			for i := len(h) - 1; i > 0; {
				p := (i - 1) / 2
				if !better(h[p], h[i]) {
					break
				}
				h[i], h[p] = h[p], h[i]
				i = p
			}
			continue
		}
		if better(x, h[0]) {
			h[0] = x
			down(0)
		}
	}
	sort.Slice(h, func(i, j int) bool { return better(h[i], h[j]) })
	return h
}
