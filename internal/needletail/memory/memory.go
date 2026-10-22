// Package memory is the agent's memory (docs/needletail.md §4.6): every memory is a record in
// memories/RECORDS.md (plain, readable, the runtime's source of truth), searched through an
// in-memory BM25 index; the records marked hot are rendered to the two hot files the system
// prompt shows, memories/MEMORY.md (target "memory") and memories/USER.md (target "user"), entries
// joined by "\n§\n", the same format as Hermes, so nap, rehydrate and the dream read them alike.
// Budgets are counted in UTF-16 units of the whole hot file.
package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// beforeWrite, when set (tests only), runs before each file a change writes: an error stands for
// a crash there.
var beforeWrite func(rel string) error

// Delimiter separates entries (a frozen contract, §8).
const Delimiter = "\n§\n"

// Targets and their files, relative to the home.
const (
	TargetMemory = "memory"
	TargetUser   = "user"
)

var files = map[string]string{TargetMemory: "memories/MEMORY.md", TargetUser: "memories/USER.md"}

var targets = []string{TargetMemory, TargetUser}

// File is a target's path relative to the home.
func File(target string) (string, bool) { f, ok := files[target]; return f, ok }

// DefaultMaxRecords is the most records an agent keeps (MaxRecords 0).
const DefaultMaxRecords = 20000

// Store is one home's memory. The zero value with Home set works: the records are read on first
// use and read again whenever one of the three files changed on disk (another process, the merge
// at boot), reconciled with the hot files (reconcile.go).
type Store struct {
	Home   string
	Limits map[string]int // per target, UTF-16 units; 0 is unlimited
	// Check refuses text holding a secret value the runtime knows (tools.SecretCheck); nil: none.
	Check func(string) error
	// MaxRecords bounds the records (0: DefaultMaxRecords).
	MaxRecords int
	// Where is a session's audience (scope.go), the runtime's: the Slack gateway's DMs and
	// threads by the session's root and source. nil: every session has none (agent-wide
	// records only).
	Where func(session string) Audience
	Now   func() time.Time
	Log   func(string)

	mu         sync.Mutex
	st         *state
	lk         *os.File // the home's lock file (lock.go), open once
	lockWarned bool
	srcCache   map[string]Audience // Where of the sessions older records name (scopeOf)
}

// state is the records as last read or written.
type state struct {
	version int
	recs    []*Record // file order
	byID    map[string]*Record
	bad     []string
	files   map[string][]string // the hot files' entries
	idx     *index
	prints  [3]fprint
	use     *usage
	// tombs are the tombstones: target + "\x00" + hash of a deleted text's normalized form → when
	// (see bury)
	tombs map[string]string
}

// Units is s's length in UTF-16 code units.
func Units(s string) int { return len(utf16.Encode([]rune(s))) }

// space is what an entry is trimmed of: every Unicode space and line terminator and the BOM, the
// set the dream and rehydrate trim (pkg/learning), so the runtime, the ledger and the merge at boot
// see the same entries. (strings.TrimSpace differs: it also trims U+0085 and not U+FEFF.)
const space = "\u0009\u000a\u000b\u000c\u000d\u0020\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"

func trim(s string) string { return strings.Trim(s, space) }

// Parse splits a file into its entries, dropping empty ones.
func Parse(raw string) []string {
	out := []string{}
	for _, e := range strings.Split(raw, Delimiter) {
		if e = trim(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// cleanEntries is entries without the bare § lines at their edges (stripBare), an entry left empty
// dropped, and how many changed.
func cleanEntries(entries []string) ([]string, int) {
	n := 0
	out := entries[:0:0]
	for _, e := range entries {
		c, changed := stripBare(e)
		if changed {
			n++
		}
		if c != "" {
			out = append(out, c)
		}
	}
	return out, n
}

// Format joins entries as the file stores them.
func Format(entries []string) string { return strings.Join(entries, Delimiter) }

func (s *Store) path(target string) (string, error) {
	f, ok := files[target]
	if !ok {
		return "", fmt.Errorf("target must be %q or %q", TargetMemory, TargetUser)
	}
	return filepath.Join(s.Home, filepath.FromSlash(f)), nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) logf(format string, args ...any) {
	if s.Log != nil {
		s.Log(fmt.Sprintf(format, args...))
	}
}

func (s *Store) tmp() string { return filepath.Join(s.Home, fsx.TmpDir) }

func (s *Store) recordsPath() string { return filepath.Join(s.Home, filepath.FromSlash(RecordsFile)) }

func (s *Store) printPaths() [3]string {
	return [3]string{s.recordsPath(), filepath.Join(s.Home, "memories", "MEMORY.md"), filepath.Join(s.Home, "memories", "USER.md")}
}

// prints is the three files' generations now.
func (s *Store) prints() [3]fprint {
	var out [3]fprint
	for i, p := range s.printPaths() {
		out[i] = statPrint(p)
	}
	return out
}

// Open reads the records and reconciles them with the hot files now (boot), instead of at first use.
func (s *Store) Open() error {
	defer s.lock()()
	_, err := s.load()
	return err
}

// load is the current state: read again when a file changed since it was last read or written.
// The caller holds the lock (lock), so no other process writes between the read and the change.
func (s *Store) load() (*state, error) {
	if s.st != nil && s.st.prints == s.prints() {
		return s.st, nil
	}
	now := s.now()
	paths := s.printPaths()
	var read [3]fprint // the generations read, kept unless load writes over them
	raw, fp, err := readGen(paths[0])
	if err != nil {
		return nil, err
	}
	read[0] = fp
	missing := !fp.ok
	parsed := ParseRecords(string(raw), now)
	st := &state{version: parsed.Version, recs: parsed.Recs, bad: parsed.Bad, files: map[string][]string{}, tombs: map[string]string{}}
	for _, tb := range parsed.Tombs {
		st.tombs[tb.Target+"\x00"+tb.Hash] = tb.At
	}
	if s.st != nil {
		st.use = s.st.use
	} else {
		st.use = loadUsage(s.Home)
	}
	dirty := map[string]bool{} // hot files to write again
	for i, t := range targets {
		body, fp, err := readGen(paths[i+1])
		if err != nil {
			return nil, err
		}
		read[i+1] = fp
		entries, cleaned := cleanEntries(Parse(string(body)))
		st.files[t] = entries
		if cleaned > 0 {
			dirty[t] = true
			s.logf("memory: %d entries of %s had a line holding only § at an edge (an empty entry, or a § first or last): dropped", cleaned, files[t])
		}
	}
	if len(parsed.Bad) > 0 {
		s.logf("memory: %d entries of %s are not records (kept as they are, after the records): fix or remove them by hand", len(parsed.Bad), RecordsFile)
	}
	write := false
	if st.version == RecordsVersion && (parsed.Repaired > 0 || parsed.Renamed > 0) {
		write = true
		s.logf("memory: %s mended: %d damaged entries repaired, %d repeated ids renamed", RecordsFile, parsed.Repaired, parsed.Renamed)
	}
	if st.version == RecordsVersion {
		src := "boot"
		if missing {
			src = "migration"
		}
		before := map[string]int{}
		for _, t := range targets {
			before[t] = len(st.files[t])
		}
		var rep Reconciled
		st.recs, rep = reconcile(st.recs, st.files, st.tombs, src, now)
		if rep.Changed {
			write = write || !missing || len(st.recs) > 0
		}
		for _, t := range targets {
			if len(st.files[t]) != before[t] {
				dirty[t] = true
			}
		}
		if rep.Created > 0 || rep.Demoted > 0 || rep.Rewritten > 0 || rep.Buried > 0 {
			s.logf("memory: hot files reconciled with %s: %d records made from hot entries (%s), %d rewritten as the hot file has them, %d not in their hot file moved to the archive (a nap skew, or an entry a reviewer rejected), %d deleted ones dropped from their hot file (a tombstone: a nap skew, or a crash)",
				RecordsFile, rep.Created, src, rep.Rewritten, rep.Demoted, rep.Buried)
		}
	} else {
		dirty = nil
		s.logf("memory: %s is format v%d, newer than this runtime's v%d: memory is read-only until it runs a newer image", RecordsFile, st.version, RecordsVersion)
	}
	st.byID = make(map[string]*Record, len(st.recs))
	st.idx = newIndex()
	for _, r := range st.recs {
		st.byID[r.ID] = r
		st.idx.add(r)
	}
	s.st = st
	st.prints = read
	if write {
		if err := s.write(RecordsFile, []byte(st.format(now))); err != nil {
			s.logf("memory: could not write %s: %v", RecordsFile, err)
		} else {
			st.prints[0] = statPrint(paths[0])
		}
	}
	for i, t := range targets {
		if !dirty[t] {
			continue
		}
		entries := texts(st.hot(t))
		if err := s.write(files[t], []byte(Format(entries))); err != nil {
			s.logf("memory: could not write %s: %v", files[t], err)
			continue
		}
		st.files[t] = entries
		st.prints[i+1] = statPrint(paths[i+1])
	}
	return st, nil
}

// format is RECORDS.md's text for st, its tombstones pruned as of now.
func (st *state) format(now time.Time) string {
	pruneTombs(st.tombs, now)
	tombs := make([]Tomb, 0, len(st.tombs))
	for k, at := range st.tombs {
		target, hash, _ := strings.Cut(k, "\x00")
		tombs = append(tombs, Tomb{Target: target, Hash: hash, At: at})
	}
	sort.Slice(tombs, func(i, j int) bool {
		if tombs[i].At != tombs[j].At {
			return tombs[i].At < tombs[j].At
		}
		return tombs[i].Target+tombs[i].Hash < tombs[j].Target+tombs[j].Hash
	})
	return FormatRecords(st.recs, st.bad, tombs...)
}

// writable is the state for a change: refused when the file's format is newer than this runtime's.
func (s *Store) writable() (*state, error) {
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	if st.version != RecordsVersion {
		return nil, fmt.Errorf("%s is format v%d, newer than this runtime reads: memory is read-only", RecordsFile, st.version)
	}
	return st, nil
}

// commit writes RECORDS.md and the hot files of targets. When a hot file loses an entry (a
// remove, a delete, an unpin, a replace, a kind move) the hot file goes first: a crash between
// the two writes leaves the record hot in RECORDS.md and missing from its hot file, which the
// next load archives (reconcile), never an entry back in the hot file with no record, which it
// would make a record again. A change that only adds writes RECORDS.md first: a crash leaves the
// new record archived. A write that fails leaves the state to be read again from disk.
func (s *Store) commit(st *state, targets ...string) error {
	now := s.now()
	paths := s.printPaths()
	hotFirst := false
	rendered := map[string][]string{}
	for _, t := range targets {
		if _, ok := files[t]; !ok {
			continue
		}
		rendered[t] = texts(st.hot(t))
		for _, e := range st.files[t] {
			if !slices.Contains(rendered[t], e) {
				hotFirst = true
			}
		}
	}
	writeHot := func() error {
		for _, t := range targets {
			entries, ok := rendered[t]
			if !ok {
				continue
			}
			// 0640: the runtime's, readable by the agent's commands (the uid split, docs/needletail.md §6)
			if err := s.write(files[t], []byte(Format(entries))); err != nil {
				return err
			}
			st.files[t] = entries
		}
		return nil
	}
	writeRecords := func() error { return s.write(RecordsFile, []byte(st.format(now))) }
	steps := []func() error{writeRecords, writeHot}
	if hotFirst {
		steps = []func() error{writeHot, writeRecords}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			s.st = nil
			return err
		}
	}
	for i, p := range paths {
		st.prints[i] = statPrint(p)
	}
	s.flushUsage(st, false)
	return nil
}

func (s *Store) flushUsage(st *state, force bool) {
	if err := st.use.write(s.Home, s.now(), force); err != nil {
		s.logf("memory: could not write %s: %v", UsageFile, err)
	}
}

// Flush writes the usage counters now (shutdown).
func (s *Store) Flush() {
	defer s.lock()()
	if s.st != nil {
		s.flushUsage(s.st, true)
	}
}

// hot is target's hot records in order.
func (st *state) hot(target string) []*Record {
	out := []*Record{}
	for _, r := range st.recs {
		if r.Hot > 0 && r.Target() == target {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Hot < out[j].Hot })
	return out
}

// renumber makes target's hot positions 1..n in their order.
func (st *state) renumber(target string) {
	for i, r := range st.hot(target) {
		r.Hot = i + 1
	}
}

func texts(recs []*Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Text
	}
	return out
}

func (st *state) create(kind, text, src string, now time.Time) *Record {
	r := &Record{ID: newID(now, func(id string) bool { return st.byID[id] != nil }), Kind: kind, Tags: []string{}, Imp: 3, Src: src,
		Created: stamp(now), Updated: stamp(now), Text: text, PII: PII(text)}
	st.recs = append(st.recs, r)
	st.byID[r.ID] = r
	return r
}

// bury leaves a tombstone for key (a normalized text) in target, unless a record of target still
// has that text: it was deleted, or replaced by another.
func (st *state) bury(target, key string, now time.Time) {
	for _, r := range st.recs {
		if r.Target() == target && r.key() == key {
			return
		}
	}
	st.tombs[target+"\x00"+TextHash(key)] = stamp(now)
}

// unbury lifts text's tombstone in target: it is saved again, on purpose.
func (st *state) unbury(target, text string) { delete(st.tombs, target+"\x00"+TextHash(text)) }

func (st *state) remove(r *Record) {
	st.recs = slices.DeleteFunc(st.recs, func(x *Record) bool { return x == r })
	delete(st.byID, r.ID)
	st.idx.remove(r.ID)
	st.use.drop(r.ID)
}

func (s *Store) maxRecords() int {
	if s.MaxRecords > 0 {
		return s.MaxRecords
	}
	return DefaultMaxRecords
}

func (s *Store) full(st *state) error {
	if len(st.recs) >= s.maxRecords() {
		return fmt.Errorf("memory holds %d records, its most: delete or merge records first", len(st.recs))
	}
	return nil
}

func (s *Store) check(text string) error {
	if s.Check != nil {
		return s.Check(text)
	}
	return nil
}

// Entries is a target's entries.
func (s *Store) Entries(target string) ([]string, error) {
	if _, err := s.path(target); err != nil {
		return nil, err
	}
	defer s.lock()()
	st, err := s.load()
	if err != nil {
		return nil, err
	}
	return slices.Clone(st.files[target]), nil
}

// Usage is a target's file size against its budget.
type Usage struct {
	Target  string `json:"target"`
	Entries int    `json:"entries"`
	Used    int    `json:"used"`
	Limit   int    `json:"limit,omitempty"`
}

func (u Usage) String() string {
	if u.Limit > 0 {
		return fmt.Sprintf("%d/%d", u.Used, u.Limit)
	}
	return fmt.Sprint(u.Used)
}

// OverBudget is a change refused because the file would pass its budget.
type OverBudget struct{ Usage Usage }

func (e *OverBudget) Error() string {
	return fmt.Sprintf("%s memory would use %d of %d UTF-16 units: replace or remove entries first", e.Usage.Target, e.Usage.Used, e.Usage.Limit)
}

// Errors.
var (
	ErrNoMatch   = errors.New("no entry contains old_text")
	ErrAmbiguous = errors.New("old_text matches more than one entry: give more of it")
	ErrEmpty     = errors.New("content is empty")
	ErrDelimiter = errors.New("content may not contain the entry separator (a line holding only §)")
	ErrNotFound  = errors.New("no memory has that id (memory_search finds ids)")
)

func (s *Store) usage(target string, entries []string) Usage {
	return Usage{Target: target, Entries: len(entries), Used: Units(Format(entries)), Limit: s.Limits[target]}
}

// fits is the usage after a change from before to after, refused when it passes the budget (a
// change that shrinks the file is always allowed).
func (s *Store) fits(target string, before, after []string) (Usage, error) {
	was, now := s.usage(target, before), s.usage(target, after)
	if now.Limit > 0 && now.Used > now.Limit && now.Used > was.Used {
		return was, &OverBudget{Usage: now}
	}
	return now, nil
}

// Current is a target's usage now.
func (s *Store) Current(target string) (Usage, error) {
	e, err := s.Entries(target)
	if err != nil {
		return Usage{}, err
	}
	return s.usage(target, e), nil
}

// Add appends an entry to a hot file (the memory tool), with no audience.
func (s *Store) Add(target, content string) (Usage, error) {
	return s.AddAs(Audience{}, "", target, content)
}

// AddAs is Add in a's turn, with the record's source (chat:<session>, …). An entry already in the
// file is a no-op; one the archive holds that a may see (the same normalized text) is pinned
// rather than copied. A new record's scope is the conversation's (none known: its src's), or
// agent-wide without one: hot, it is in every system prompt anyway (Hermes' memory tool).
func (s *Store) AddAs(a Audience, src, target, content string) (Usage, error) {
	if _, err := s.path(target); err != nil {
		return Usage{}, err
	}
	c, err := s.hotText(content)
	if err != nil {
		return Usage{}, err
	}
	defer s.lock()()
	st, err := s.writable()
	if err != nil {
		return Usage{}, err
	}
	list := st.hot(target)
	before := texts(list)
	if slices.Contains(before, c) {
		return s.usage(target, before), nil // already there
	}
	r := st.archived(target, Normalize(c), func(r *Record) bool { return s.visible(r, a, false) })
	if r == nil {
		if err := s.full(st); err != nil {
			return s.usage(target, before), err
		}
	}
	u, err := s.fits(target, before, append(slices.Clone(before), c))
	if err != nil {
		return u, err
	}
	now := s.now()
	if r == nil {
		kind := KindFact
		if target == TargetUser {
			kind = KindUser
		}
		r = st.create(kind, c, src, now)
		r.Scope, _ = saveScope("", a, kind, len(PII(c)) > 0, true)
	}
	r.Text, r.PII, r.Updated, r.Hot = c, PII(c), stamp(now), len(before)+1
	st.idx.add(r)
	st.unbury(target, c)
	return u, s.commit(st, target)
}

// archived is the archive record of target with Normalize(text) key that ok takes, the latest
// updated.
func (st *state) archived(target, key string, ok func(*Record) bool) *Record {
	var best *Record
	for _, r := range st.recs {
		if r.Hot == 0 && r.Target() == target && r.key() == key && (best == nil || r.Updated > best.Updated) && ok(r) {
			best = r
		}
	}
	return best
}

// hotText is content cleaned and checked for a hot file.
func (s *Store) hotText(content string) (string, error) {
	c, err := clean(content)
	if err != nil {
		return "", err
	}
	if err := checkText(c, false); err != nil {
		return "", err
	}
	if err := checkHot(c); err != nil {
		return "", err
	}
	if err := checkSecret(c); err != nil {
		return "", err
	}
	if err := s.check(c); err != nil {
		return "", err
	}
	return c, nil
}

// Replace swaps the one entry containing old for content.
func (s *Store) Replace(target, old, content string) (Usage, error) {
	if _, err := s.path(target); err != nil {
		return Usage{}, err
	}
	c, err := s.hotText(content)
	if err != nil {
		return Usage{}, err
	}
	defer s.lock()()
	st, err := s.writable()
	if err != nil {
		return Usage{}, err
	}
	list := st.hot(target)
	before := texts(list)
	i, err := match(before, old)
	if err != nil {
		return s.usage(target, before), err
	}
	after := slices.Clone(before)
	after[i] = c
	u, err := s.fits(target, before, after)
	if err != nil {
		return u, err
	}
	r := list[i]
	was := r.key()
	now := s.now()
	r.Text, r.PII, r.Updated = c, PII(c), stamp(now)
	st.idx.add(r)
	st.bury(target, was, now)
	st.unbury(target, c)
	return u, s.commit(st, target)
}

// Remove deletes the one entry containing old (its record goes too).
func (s *Store) Remove(target, old string) (Usage, error) {
	if _, err := s.path(target); err != nil {
		return Usage{}, err
	}
	defer s.lock()()
	st, err := s.writable()
	if err != nil {
		return Usage{}, err
	}
	list := st.hot(target)
	before := texts(list)
	i, err := match(before, old)
	if err != nil {
		return s.usage(target, before), err
	}
	st.remove(list[i])
	st.bury(target, list[i].key(), s.now())
	st.renumber(target)
	return s.usage(target, texts(st.hot(target))), s.commit(st, target)
}

func clean(content string) (string, error) {
	c := trim(content)
	if c == "" {
		return "", ErrEmpty
	}
	if strings.Contains("\n"+c+"\n", Delimiter) {
		return "", ErrDelimiter
	}
	return c, nil
}

func match(entries []string, old string) (int, error) {
	old = trim(old)
	if old == "" {
		return -1, errors.New("old_text is empty")
	}
	found := -1
	for i, e := range entries {
		if strings.Contains(e, old) {
			if found >= 0 {
				return -1, ErrAmbiguous
			}
			found = i
		}
	}
	if found < 0 {
		return -1, ErrNoMatch
	}
	return found, nil
}

// Block is a target's entries as the system prompt shows them ("" when empty).
func (s *Store) Block(target string) string {
	e, err := s.Entries(target)
	if err != nil || len(e) == 0 {
		return ""
	}
	return strings.Join(e, "\n§\n")
}
