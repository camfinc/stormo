package memory

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// The archive's CRUD by id (memory_manage, Luau's save/update/delete_memory; docs/needletail.md
// §4.6).

// SaveOptions is a new record.
type SaveOptions struct {
	Text string
	// Kind is fact (default), preference, procedure, episode or user.
	Kind string
	Tags []string
	// Importance is 1 to 5 (0: 3).
	Importance int
	// Expires is a date, YYYY-MM-DD ("": none).
	Expires string
	// Hot saves it straight into its hot file (budget permitting); Force saves it although
	// similar records exist.
	Hot, Force bool
	Src        string
}

// Similar is a record a save found too close to its text.
type Similar struct {
	ID    string  `json:"id"`
	Text  string  `json:"text,omitempty"`
	Score float64 `json:"score"`
}

// SaveResult is a save's answer: created (ID the new record), exists (ID the record with the same
// normalized text, touched) or similar (nothing saved: update one of Similar, or save with force).
type SaveResult struct {
	Status  string    `json:"status"`
	ID      string    `json:"id,omitempty"`
	Hot     int       `json:"hot,omitempty"`
	Similar []Similar `json:"similar,omitempty"`
	Usage   string    `json:"usage,omitempty"`
	PII     []string  `json:"pii,omitempty"`
}

// Save statuses.
const (
	StatusCreated = "created"
	StatusExists  = "exists"
	StatusSimilar = "similar"
)

// similarAt is the token containment, either way, at which a saved text is a near duplicate.
const similarAt = 0.75

func checkKind(kind string) (string, error) {
	if kind == "" {
		return KindFact, nil
	}
	if !slices.Contains(Kinds, kind) {
		return "", fmt.Errorf("kind %q: one of %s", kind, strings.Join(Kinds, ", "))
	}
	return kind, nil
}

func checkImportance(imp int) (int, error) {
	if imp == 0 {
		return 3, nil
	}
	if imp < 1 || imp > 5 {
		return 0, fmt.Errorf("importance %d: 1 (minor) to 5 (essential)", imp)
	}
	return imp, nil
}

func checkExpires(d string) (string, error) {
	d = strings.TrimSpace(d)
	if d == "" {
		return "", nil
	}
	if _, err := time.Parse(time.DateOnly, d); err != nil {
		return "", fmt.Errorf("expires %q: a date, YYYY-MM-DD", d)
	}
	return d, nil
}

// recordText is text cleaned and checked for a record (hot: also for the system prompt).
func (s *Store) recordText(text string, hot bool) (string, error) {
	c, err := clean(text)
	if err != nil {
		return "", err
	}
	if err := checkText(c, true); err != nil {
		return "", err
	}
	if hot {
		if err := checkHot(c); err != nil {
			return "", err
		}
	}
	if err := s.check(c); err != nil {
		return "", err
	}
	return c, nil
}

// Save makes a record, unless one with the same normalized text exists (exists) or, without
// Force, similar ones do (similar: nothing saved).
func (s *Store) Save(o SaveOptions) (SaveResult, error) {
	c, err := s.recordText(o.Text, o.Hot)
	if err != nil {
		return SaveResult{}, err
	}
	kind, err := checkKind(o.Kind)
	if err != nil {
		return SaveResult{}, err
	}
	tags, err := cleanTags(o.Tags)
	if err != nil {
		return SaveResult{}, err
	}
	imp, err := checkImportance(o.Importance)
	if err != nil {
		return SaveResult{}, err
	}
	exp, err := checkExpires(o.Expires)
	if err != nil {
		return SaveResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.writable()
	if err != nil {
		return SaveResult{}, err
	}
	now := s.now()
	target := targetOf(kind)
	key := Normalize(c)
	for _, r := range st.recs {
		if r.Target() == target && Normalize(r.Text) == key {
			r.Updated = stamp(now)
			return SaveResult{Status: StatusExists, ID: r.ID, Hot: r.Hot}, s.commit(st)
		}
	}
	if !o.Force {
		if sim := st.similar(c, 5); len(sim) > 0 {
			return SaveResult{Status: StatusSimilar, Similar: sim}, nil
		}
	}
	if err := s.full(st); err != nil {
		return SaveResult{}, err
	}
	res := SaveResult{Status: StatusCreated}
	var hot []string
	if o.Hot {
		before := texts(st.hot(target))
		u, err := s.fits(target, before, append(slices.Clone(before), c))
		if err != nil {
			return SaveResult{}, fmt.Errorf("%w (now %s); save it without hot, or unpin or shorten another first", err, u)
		}
		res.Usage = u.String()
		hot = []string{target}
	}
	r := st.create(kind, c, o.Src, now)
	r.Tags, r.Imp, r.Expires = tags, imp, exp
	if o.Hot {
		r.Hot = len(st.hot(target)) + 1
	}
	st.idx.add(r)
	res.ID, res.Hot, res.PII = r.ID, r.Hot, r.PII
	return res, s.commit(st, hot...)
}

// similar is the records (at most n) whose tokens contain text's, or are contained in them, at
// similarAt or more.
func (st *state) similar(text string, n int) []Similar {
	q := distinct(tokens(text))
	if len(q) == 0 {
		return nil
	}
	terms := make([]string, 0, len(q))
	for t := range q {
		terms = append(terms, t)
	}
	hits := st.idx.score(terms, 0)
	type cand struct {
		d     *doc
		c, bm float64
	}
	cands := []cand{}
	for _, h := range hits {
		d := distinct(tokens(h.doc.rec.Text))
		inter := 0
		for t := range q {
			if d[t] {
				inter++
			}
		}
		if len(d) == 0 {
			continue
		}
		c := max(float64(inter)/float64(len(q)), float64(inter)/float64(len(d)))
		if c >= similarAt {
			cands = append(cands, cand{h.doc, c, h.score})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].c != cands[j].c {
			return cands[i].c > cands[j].c
		}
		return cands[i].bm > cands[j].bm
	})
	out := []Similar{}
	for _, c := range cands {
		if len(out) == n {
			break
		}
		out = append(out, Similar{ID: c.d.rec.ID, Text: cut(c.d.rec.Text, searchCut), Score: round(c.c)})
	}
	return out
}

// Change is an update: the fields set change.
type Change struct {
	Text       *string
	Kind       *string
	Tags       *[]string
	Importance *int
	Expires    *string
}

// ErrHotRecord refuses a script's change to a hot record (those are the model's).
var ErrHotRecord = errors.New("the record is hot (in the system prompt): only the model changes it (memory_manage)")

// Update changes a record. A hot record is checked against its budget and re-rendered; a kind
// change between user and the others moves it between USER.md and MEMORY.md. archiveOnly refuses
// hot records (Luau).
func (s *Store) Update(id string, ch Change, archiveOnly bool) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.writable()
	if err != nil {
		return View{}, err
	}
	r := st.byID[id]
	if r == nil {
		return View{}, ErrNotFound
	}
	if archiveOnly && r.Hot > 0 {
		return View{}, ErrHotRecord
	}
	n := r.clone()
	if ch.Text != nil {
		if n.Text, err = s.recordText(*ch.Text, r.Hot > 0); err != nil {
			return View{}, err
		}
	}
	if ch.Kind != nil {
		if n.Kind, err = checkKind(*ch.Kind); err != nil {
			return View{}, err
		}
	}
	if ch.Tags != nil {
		if n.Tags, err = cleanTags(*ch.Tags); err != nil {
			return View{}, err
		}
	}
	if ch.Importance != nil {
		if n.Imp, err = checkImportance(*ch.Importance); err != nil {
			return View{}, err
		}
	}
	if ch.Expires != nil {
		if n.Expires, err = checkExpires(*ch.Expires); err != nil {
			return View{}, err
		}
	}
	var changed []string
	if r.Hot > 0 {
		from, to := r.Target(), n.Target()
		before := texts(st.hot(to))
		var after []string
		if from == to {
			after = slices.Clone(before)
			after[r.Hot-1] = n.Text
		} else {
			after = append(slices.Clone(before), n.Text)
			n.Hot = len(before) + 1
		}
		if _, err := s.fits(to, before, after); err != nil {
			return View{}, err
		}
		changed = append(changed, to)
		if from != to {
			changed = append(changed, from)
		}
	}
	now := s.now()
	n.PII, n.Updated = PII(n.Text), stamp(now)
	*r = *n
	for _, t := range changed {
		st.renumber(t)
	}
	st.idx.add(r)
	st.use.used(r.ID, now)
	return st.view(r, now, 0, false), s.commit(st, changed...)
}

// Delete removes a record (from its hot file too). archiveOnly refuses hot records (Luau).
func (s *Store) Delete(id string, archiveOnly bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.writable()
	if err != nil {
		return err
	}
	r := st.byID[id]
	if r == nil {
		return ErrNotFound
	}
	if archiveOnly && r.Hot > 0 {
		return ErrHotRecord
	}
	var changed []string
	if r.Hot > 0 {
		changed = []string{r.Target()}
	}
	st.remove(r)
	for _, t := range changed {
		st.renumber(t)
	}
	return s.commit(st, changed...)
}

// Pin moves an archive record to the end of its hot file (refused over budget); Unpin moves a hot
// one back to the archive.
func (s *Store) Pin(id string) (View, Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.writable()
	if err != nil {
		return View{}, Usage{}, err
	}
	r := st.byID[id]
	if r == nil {
		return View{}, Usage{}, ErrNotFound
	}
	now := s.now()
	t := r.Target()
	before := texts(st.hot(t))
	if r.Hot > 0 {
		return st.view(r, now, 0, false), s.usage(t, before), nil
	}
	if err := checkHot(r.Text); err != nil {
		return View{}, Usage{}, err
	}
	u, err := s.fits(t, before, append(slices.Clone(before), r.Text))
	if err != nil {
		return View{}, u, fmt.Errorf("%w (now %s); unpin or shorten another first", err, u)
	}
	r.Hot = len(before) + 1
	st.use.used(r.ID, now)
	return st.view(r, now, 0, false), u, s.commit(st, t)
}

// Unpin moves a hot record back to the archive.
func (s *Store) Unpin(id string) (View, Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.writable()
	if err != nil {
		return View{}, Usage{}, err
	}
	r := st.byID[id]
	if r == nil {
		return View{}, Usage{}, ErrNotFound
	}
	now := s.now()
	t := r.Target()
	if r.Hot == 0 {
		return st.view(r, now, 0, false), s.usage(t, texts(st.hot(t))), nil
	}
	r.Hot = 0
	st.renumber(t)
	u := s.usage(t, texts(st.hot(t)))
	return st.view(r, now, 0, false), u, s.commit(st, t)
}
