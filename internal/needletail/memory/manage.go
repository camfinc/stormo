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
	// Audience is the turn's (the runtime's); Scope is "here" (its conversation, the default) or
	// "agent" (everyone): see saveScope.
	Audience Audience
	Scope    string
	// ArchiveOnly is a script's save (Luau): its text is checked as a hot one is (a script's text
	// is the least trusted that reaches recall), and a hot record with the same text is left as it
	// is (exists, uncounted).
	ArchiveOnly bool
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
	Scope   string    `json:"scope,omitempty"`
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

// recordText is text cleaned and checked for a record (strict: also for instructions, as a hot
// text or a script's is).
func (s *Store) recordText(text string, strict bool) (string, error) {
	c, err := clean(text)
	if err != nil {
		return "", err
	}
	if err := checkText(c, true); err != nil {
		return "", err
	}
	if strict {
		if err := checkHot(c); err != nil {
			return "", err
		}
	}
	if err := checkSecret(c); err != nil {
		return "", err
	}
	if err := s.check(c); err != nil {
		return "", err
	}
	return c, nil
}

// Save makes a record, unless one with the same normalized text exists (exists) or, without
// Force, similar ones do (similar: nothing saved).
func (s *Store) Save(o SaveOptions) (SaveResult, error) {
	c, err := s.recordText(o.Text, o.Hot || o.ArchiveOnly)
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
	if err := checkSecret(tags...); err != nil {
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
	scope, err := saveScope(o.Scope, o.Audience, kind, len(PII(c)) > 0, o.Hot)
	if err != nil {
		return SaveResult{}, err
	}
	defer s.lock()()
	st, err := s.writable()
	if err != nil {
		return SaveResult{}, err
	}
	now := s.now()
	target := targetOf(kind)
	key := Normalize(c)
	seen := func(r *Record) bool { return s.visible(r, o.Audience, false) }
	for _, r := range st.recs {
		if r.Target() == target && r.key() == key && seen(r) {
			// a use, counted apart (no rewrite of RECORDS.md for a timestamp); a script's save of a
			// hot record's text leaves it as it is
			if !(o.ArchiveOnly && r.Hot > 0) {
				st.use.used(r.ID, now)
				s.flushUsage(st, false)
			}
			return SaveResult{Status: StatusExists, ID: r.ID, Hot: r.Hot, Scope: r.Scope}, nil
		}
	}
	if !o.Force {
		if sim := st.similar(c, 5, seen); len(sim) > 0 {
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
	st.unbury(target, c)
	r.Tags, r.Imp, r.Expires, r.Scope = tags, imp, exp, scope
	if o.Hot {
		r.Hot = len(st.hot(target)) + 1
	}
	st.idx.add(r)
	res.ID, res.Hot, res.PII, res.Scope = r.ID, r.Hot, r.PII, r.Scope
	return res, s.commit(st, hot...)
}

// similar is the records ok takes (at most n) whose tokens contain text's, or are contained in
// them, at similarAt or more.
func (st *state) similar(text string, n int, ok func(*Record) bool) []Similar {
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
		// containment from the index's own terms of the text (no text tokenised again; a tag's
		// terms do not count)
		// h.matched (text or tags) bounds what the text shares: a hit that cannot reach the
		// threshold is passed over before anything else is looked at
		d := h.doc
		if d.nText == 0 || max(float64(h.matched)/float64(len(q)), float64(h.matched)/float64(d.nText)) < similarAt || !ok(d.rec) {
			continue
		}
		inter := 0
		for _, t := range terms {
			if d.has(t) {
				inter++
			}
		}
		c := max(float64(inter)/float64(len(q)), float64(inter)/float64(d.nText))
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

// byID is the record id names that a may see (ErrNotFound for one it may not: its existence is
// not a's to learn either). The owner's own runs reach any record by id.
func (s *Store) byID(st *state, a Audience, id string) (*Record, error) {
	r := st.byID[id]
	if r == nil || !s.visible(r, a, a.Operator) {
		return nil, ErrNotFound
	}
	return r, nil
}

// Update changes a record a may see. A hot record is checked against its budget and re-rendered;
// a kind change between user and the others moves it between USER.md and MEMORY.md. archiveOnly
// refuses hot records (Luau).
func (s *Store) Update(a Audience, id string, ch Change, archiveOnly bool) (View, error) {
	defer s.lock()()
	st, err := s.writable()
	if err != nil {
		return View{}, err
	}
	r, err := s.byID(st, a, id)
	if err != nil {
		return View{}, err
	}
	if archiveOnly && r.Hot > 0 {
		return View{}, ErrHotRecord
	}
	n := r.clone()
	if ch.Text != nil {
		if n.Text, err = s.recordText(*ch.Text, r.Hot > 0 || archiveOnly); err != nil {
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
		if err := checkSecret(n.Tags...); err != nil {
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
	if r.Hot == 0 && n.Scope == ScopeAgent && !agentWide(a, n.Kind, len(n.PII) > 0) {
		return View{}, ErrAgentWide
	}
	wasTarget, wasKey := r.Target(), r.key()
	*r = *n
	st.bury(wasTarget, wasKey, now)
	st.unbury(r.Target(), r.Text)
	for _, t := range changed {
		st.renumber(t)
	}
	st.idx.add(r)
	st.use.used(r.ID, now)
	return st.view(r, now, 0, false), s.commit(st, changed...)
}

// Delete removes a record a may see (from its hot file too). archiveOnly refuses hot records
// (Luau).
func (s *Store) Delete(a Audience, id string, archiveOnly bool) error {
	defer s.lock()()
	st, err := s.writable()
	if err != nil {
		return err
	}
	r, err := s.byID(st, a, id)
	if err != nil {
		return err
	}
	if archiveOnly && r.Hot > 0 {
		return ErrHotRecord
	}
	var changed []string
	if r.Hot > 0 {
		changed = []string{r.Target()}
	}
	st.remove(r)
	st.bury(r.Target(), r.key(), s.now())
	for _, t := range changed {
		st.renumber(t)
	}
	return s.commit(st, changed...)
}

// Pin moves an archive record a may see to the end of its hot file (refused over budget), where
// every conversation sees it; Unpin moves a hot one back to the archive (its scope's again).
func (s *Store) Pin(a Audience, id string) (View, Usage, error) {
	defer s.lock()()
	st, err := s.writable()
	if err != nil {
		return View{}, Usage{}, err
	}
	r, err := s.byID(st, a, id)
	if err != nil {
		return View{}, Usage{}, err
	}
	now := s.now()
	t := r.Target()
	before := texts(st.hot(t))
	if r.Hot > 0 {
		return st.view(r, now, 0, false), s.usage(t, before), nil
	}
	// into every system prompt: checked as a hot save is (an older runtime's record, a hand edit)
	if err := checkHot(r.Text); err != nil {
		return View{}, Usage{}, err
	}
	if err := checkSecret(append([]string{r.Text}, r.Tags...)...); err != nil {
		return View{}, Usage{}, err
	}
	if err := s.check(r.Text); err != nil {
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
func (s *Store) Unpin(a Audience, id string) (View, Usage, error) {
	defer s.lock()()
	st, err := s.writable()
	if err != nil {
		return View{}, Usage{}, err
	}
	r, err := s.byID(st, a, id)
	if err != nil {
		return View{}, Usage{}, err
	}
	now := s.now()
	t := r.Target()
	if r.Hot == 0 {
		return st.view(r, now, 0, false), s.usage(t, texts(st.hot(t))), nil
	}
	r.Hot = 0
	if r.Scope == ScopeAgent && !agentWide(a, r.Kind, r.personal()) {
		r.Scope = "" // back in the archive, personal text is not everyone's: its src's rule decides
	}
	st.renumber(t)
	u := s.usage(t, texts(st.hot(t)))
	return st.view(r, now, 0, false), u, s.commit(st, t)
}
