// Package sessions stores conversations as JSONL files (docs/needletail.md §6): one file per
// session, sessions/<id>.jsonl, whose first line is the wire.Session header and whose other lines
// are wire.Message rows. Message lines are only ever appended and fsynced at turn boundaries; the
// header is rewritten (tmp file, fsync, rename) only when the session ends. sessions/index.json is a
// summary for readers outside the runtime, replaced by atomic rename; the runtime rebuilds its own
// index from the files at boot.
//
// A compaction ends a session (end_reason "compression") and continues the conversation in a new
// session whose parent is the old one: the lineage. The new session starts with one system row (the
// summary) followed by the recent rows it carried over, copied verbatim (timestamps included), so a
// session file alone is enough to resume it. A transcript follows the lineage from its root and
// leaves out the summaries and the carried copies.
package sessions

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Errors.
var (
	ErrNotFound = errors.New("session not found")
	ErrExists   = errors.New("session exists")
	ErrBadID    = errors.New("session id must be 1-128 of A-Z a-z 0-9 . _ : - and not start with a dot")
	ErrEnded    = errors.New("session ended")
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9_:-][A-Za-z0-9._:-]{0,127}$`)

// ValidID: ids are file names, so no separators and no leading dot.
func ValidID(id string) bool { return idRe.MatchString(id) }

// NewID is a fresh id: UTC time plus randomness, sortable.
func NewID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102_150405") + "_" + hex.EncodeToString(b)
}

// Stamp is the time format of every timestamp the store writes.
func Stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

type meta struct {
	wire.Session
	child      string
	visible    int // non-system rows in the file
	carried    int // rows copied from the parent at compaction
	lastActive string
	preview    string // first user row
}

// Store is the sessions directory. Safe for concurrent use; the caller serialises turns per
// lineage (one writer per session).
type Store struct {
	dir    string
	tmp    string // the home's unclaimed temp dir (fsx.TmpDir), beside dir
	redact func(string) string
	now    func() time.Time
	fsync  func(*os.File) error // (*os.File).Sync; tests watch or fail it

	mu    sync.Mutex
	metas map[string]*meta
	// gone are told when a conversation ends for good or is deleted (OnGone)
	gone []func(root, reason string)
}

// Reasons OnGone's functions are given besides an End's own reason.
const GoneDeleted = "deleted"

// OnGone registers f, told (outside the store's lock) when a conversation is deleted
// (DeleteLineage: reason GoneDeleted) or one of its sessions ends for good (End, with its reason:
// a scheduled run's cron_complete, a oneshot's): no turn will come back to it. A compaction's end
// is not one (the conversation goes on in its child). root is the conversation's root.
func (s *Store) OnGone(f func(root, reason string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gone = append(s.gone, f)
}

func (s *Store) tell(root, reason string, fs []func(root, reason string)) {
	for _, f := range fs {
		f(root, reason)
	}
}

// Open reads every session file in dir (created when missing). redact masks secrets in every row
// written; nil writes rows as given.
func Open(dir string, redact func(string) string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if redact == nil {
		redact = func(s string) string { return s }
	}
	s := &Store{dir: dir, tmp: filepath.Join(filepath.Dir(dir), fsx.TmpDir), redact: redact, now: time.Now, fsync: (*os.File).Sync, metas: map[string]*meta{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	msgs := map[string][]wire.Message{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !ValidID(id) || !e.Type().IsRegular() {
			continue
		}
		repair(s.path(id))
		h, rows, err := s.read(id)
		if err != nil || h.ID != id {
			continue // unreadable: left alone, never served
		}
		m := &meta{Session: h, lastActive: h.Created}
		for _, r := range rows {
			m.observe(r)
		}
		s.metas[id] = m
		msgs[id] = rows
	}
	for id, m := range s.metas {
		if p, ok := s.metas[m.Parent]; ok {
			p.child = id
			m.carried = carried(msgs[m.Parent], msgs[id])
		}
	}
	return s, nil
}

// repair drops a torn last line (a crash mid-append), so the next append starts on a fresh line.
func repair(path string) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, info.Size()-1); err != nil || last[0] == '\n' {
		return
	}
	const window = 64 << 10
	for end := info.Size(); end > 0; end -= window {
		start := max(0, end-window)
		buf := make([]byte, end-start)
		if _, err := f.ReadAt(buf, start); err != nil {
			return
		}
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			_ = f.Truncate(start + int64(i) + 1)
			return
		}
	}
	// no complete line at all: leave it (the header is unreadable; the session is not served)
}

func (m *meta) observe(r wire.Message) {
	if r.Untrusted {
		m.Untrusted = true // a row written before its header's mark (a crash between): marked all the same
	}
	if r.Role != "system" {
		m.visible++
	}
	if r.At != "" {
		m.lastActive = r.At
	}
	if m.preview == "" && r.Role == "user" {
		m.preview = preview(r.Content)
	}
}

func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

func (s *Store) path(id string) string { return filepath.Join(s.dir, id+".jsonl") }

// read is a session file: its header and rows.
func (s *Store) read(id string) (wire.Session, []wire.Message, error) {
	f, err := os.Open(s.path(id))
	if err != nil {
		return wire.Session{}, nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	var h wire.Session
	var rows []wire.Message
	first := true
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if first {
			first = false
			if err := json.Unmarshal(line, &h); err != nil || h.V != wire.SessionVersion {
				return h, nil, fmt.Errorf("%s: not a v%d session header", id, wire.SessionVersion)
			}
			continue
		}
		var m wire.Message
		if json.Unmarshal(line, &m) != nil {
			continue // a torn last line after a crash
		}
		rows = append(rows, m)
	}
	if first {
		return h, nil, fmt.Errorf("%s: empty", id)
	}
	return h, rows, sc.Err()
}

// Create starts a session. An empty id gets a fresh one.
func (s *Store) Create(h wire.Session) (wire.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.create(h)
}

func (s *Store) create(h wire.Session) (wire.Session, error) {
	if h.ID == "" {
		h.ID = NewID()
	}
	if !ValidID(h.ID) {
		return h, ErrBadID
	}
	if _, ok := s.metas[h.ID]; ok {
		return h, ErrExists
	}
	h.V = wire.SessionVersion
	if h.Created == "" {
		h.Created = Stamp(s.now())
	}
	line, _ := json.Marshal(h)
	f, err := os.OpenFile(s.path(h.ID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return h, ErrExists
		}
		return h, err
	}
	_, err = f.Write(append(line, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(s.path(h.ID))
		return h, err
	}
	s.metas[h.ID] = &meta{Session: h, lastActive: h.Created}
	return h, nil
}

// Get is a session's header.
func (s *Store) Get(id string) (wire.Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[id]
	if !ok {
		return wire.Session{}, false
	}
	return m.Session, true
}

// Lineage is the conversation id belongs to, root first, tip last.
func (s *Store) Lineage(id string) ([]string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lineage(id)
}

func (s *Store) lineage(id string) ([]string, bool) {
	m, ok := s.metas[id]
	if !ok {
		return nil, false
	}
	root := id
	for seen := 0; m.Parent != "" && seen < 10000; seen++ {
		p, ok := s.metas[m.Parent]
		if !ok {
			break
		}
		root, m = m.Parent, p
	}
	out := []string{root}
	for cur := s.metas[root]; cur.child != "" && len(out) < 10000; cur = s.metas[cur.child] {
		out = append(out, cur.child)
	}
	return out, true
}

// Root is the conversation's first session; Tip its live one.
func (s *Store) Root(id string) (string, bool) {
	l, ok := s.Lineage(id)
	if !ok {
		return "", false
	}
	return l[0], true
}

func (s *Store) Tip(id string) (string, bool) {
	l, ok := s.Lineage(id)
	if !ok {
		return "", false
	}
	return l[len(l)-1], true
}

// Untrusted is whether the conversation id belongs to has taken in content the agent must not
// obey (any session of its lineage marked, wire.Session.Untrusted). Never cleared: the owner
// starts a fresh conversation (a new thread, a new API conversation) to defer work again.
func (s *Store) Untrusted(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lineage(id)
	if !ok {
		return false
	}
	for _, sid := range l {
		if s.metas[sid].Untrusted {
			return true
		}
	}
	return false
}

// Messages is one session's rows, as stored (summary and carried rows included): what the model
// sees when the session resumes.
func (s *Store) Messages(id string) ([]wire.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.metas[id]; !ok {
		return nil, ErrNotFound
	}
	_, rows, err := s.read(id)
	return rows, err
}

// Append adds rows to a live session, redacted and stamped, and returns them as stored: a caller
// that keeps the conversation in memory keeps these, so rows it later carries into a compaction
// match the file row for row (see carried). Durable at the next Sync.
func (s *Store) Append(id string, rows ...wire.Message) ([]wire.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[id]
	if !ok {
		return nil, ErrNotFound
	}
	if m.EndReason != "" {
		return nil, ErrEnded
	}
	if !m.Untrusted && slices.ContainsFunc(rows, func(r wire.Message) bool { return r.Untrusted }) {
		// the header is marked before the row is written: a conversation never holds an
		// untrusted row it does not know of (fails closed: no mark, no row)
		if err := s.rewriteHeader(m, func(h *wire.Session) { h.Untrusted = true }); err != nil {
			return nil, err
		}
	}
	return s.append(m, rows, false)
}

// append writes rows (the caller's slice is left alone) and returns them as written.
func (s *Store) append(m *meta, in []wire.Message, keepStamp bool) ([]wire.Message, error) {
	var buf bytes.Buffer
	now := Stamp(s.now())
	rows := make([]wire.Message, len(in))
	for i := range in {
		r := in[i]
		r.Content = s.redact(r.Content)
		r.ToolName = "" // files pair tool rows by id
		if len(r.ToolCalls) > 0 {
			calls := make([]wire.ToolCall, len(r.ToolCalls))
			for j, c := range r.ToolCalls {
				c.Arguments = s.redact(c.Arguments)
				calls[j] = c
			}
			r.ToolCalls = calls
		}
		if r.At == "" || !keepStamp {
			r.At = now
		}
		line, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
		rows[i] = r
	}
	f, err := os.OpenFile(s.path(m.ID), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(buf.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		m.observe(r)
	}
	return rows, nil
}

// Sync makes a session's rows durable and refreshes sessions/index.json (a turn boundary).
func (s *Store) Sync(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.metas[id]; ok {
		if err := s.syncFile(id); err != nil {
			return err
		}
	}
	return s.writeIndex()
}

// End closes a session with a reason, rewriting its header atomically; OnGone's functions are
// told.
func (s *Store) End(id, reason string) error {
	s.mu.Lock()
	err := s.end(id, reason)
	root := id
	if l, ok := s.lineage(id); ok {
		root = l[0]
	}
	gone := s.gone
	s.mu.Unlock()
	if err == nil {
		s.tell(root, reason, gone)
	}
	return err
}

func (s *Store) end(id, reason string) error {
	m, ok := s.metas[id]
	if !ok {
		return ErrNotFound
	}
	return s.rewriteHeader(m, func(h *wire.Session) { h.EndReason = reason })
}

// rewriteHeader changes a session's header (atomically: the file is written again whole through
// the home's temp dir) and its meta; s.mu is held, so no row is appended meanwhile.
func (s *Store) rewriteHeader(m *meta, change func(*wire.Session)) error {
	raw, err := os.ReadFile(s.path(m.ID))
	if err != nil {
		return err
	}
	h := m.Session
	change(&h)
	line, _ := json.Marshal(h)
	rest := raw
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		rest = raw[i+1:]
	} else {
		rest = nil
	}
	if err := fsx.WriteAtomicIn(s.path(m.ID), s.tmp, append(append(line, '\n'), rest...), 0o600); err != nil {
		return err
	}
	m.Session = h
	return nil
}

// Compact ends id (end_reason compression) and continues it in a new session whose parent is id:
// a system row with the summary, then the carried rows verbatim. It returns the new session.
func (s *Store) Compact(id, summary string, carry []wire.Message) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.metas[id]
	if !ok {
		return "", ErrNotFound
	}
	if m.child != "" || m.EndReason != "" {
		return "", ErrEnded
	}
	child, err := s.create(wire.Session{Parent: id, Source: m.Source, Title: m.Title, Untrusted: m.Untrusted})
	if err != nil {
		return "", err
	}
	c := s.metas[child.ID]
	now := Stamp(s.now())
	rows := append([]wire.Message{{Role: "system", Content: summary, At: now}}, carry...)
	_, err = s.append(c, rows, true)
	if err == nil {
		// the new session is durable before the old one ends: a crash in between leaves the old
		// one live (and the new one dropped below), never an ended session without its sequel
		err = s.syncFile(child.ID)
	}
	if err == nil {
		err = s.end(id, wire.EndCompression)
	}
	if err != nil {
		// a child on disk with a live parent would become the conversation's tip at the next boot
		_ = os.Remove(s.path(child.ID))
		delete(s.metas, child.ID)
		return "", err
	}
	c.carried = len(carry)
	m.child = child.ID
	return child.ID, s.writeIndex()
}

func (s *Store) syncFile(id string) error {
	f, err := os.OpenFile(s.path(id), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	err = s.fsync(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// carried is how many rows at the start of child (after its summary rows) copy parent's last ones.
func carried(parent, child []wire.Message) int {
	i := 0
	for i < len(child) && child[i].Role == "system" {
		i++
	}
	c := child[i:]
	for k := min(len(parent), len(c)); k > 0; k-- {
		if same(c[:k], parent[len(parent)-k:]) {
			return k
		}
	}
	return 0
}

func same(a, b []wire.Message) bool {
	for i := range a {
		x, y := a[i], b[i]
		if x.Role != y.Role || x.Content != y.Content || x.ToolCallID != y.ToolCallID || x.At != y.At || len(x.ToolCalls) != len(y.ToolCalls) {
			return false
		}
		for j := range x.ToolCalls {
			if x.ToolCalls[j].ID != y.ToolCalls[j].ID {
				return false
			}
		}
	}
	return true
}

// List is the conversations, newest first, each as its tip.
func (s *Store) List(limit int) []wire.SessionSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.summaries()
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (s *Store) summaries() []wire.SessionSummary {
	out := []wire.SessionSummary{}
	for id, m := range s.metas {
		if m.child != "" {
			continue
		}
		l, _ := s.lineage(id)
		root := s.metas[l[0]]
		sum := wire.SessionSummary{Session: m.Session, Preview: root.preview, LastActive: m.lastActive, Ended: m.EndReason != ""}
		if l[0] != id {
			sum.Root = l[0]
		}
		for _, sid := range l {
			x := s.metas[sid]
			sum.MessageCount += x.visible - x.carried
		}
		out = append(out, sum)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastActive != out[j].LastActive {
			return out[i].LastActive > out[j].LastActive
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// Transcript is the conversation id belongs to, from its root, oldest first: summaries and
// carried copies left out, tool rows named after their calls. limit keeps the first (or, with
// latest, the last) rows.
func (s *Store) Transcript(id string, limit int, latest bool) (wire.Transcript, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lineage(id)
	if !ok {
		return wire.Transcript{}, ErrNotFound
	}
	t := wire.Transcript{SessionID: l[len(l)-1], Data: []wire.Message{}}
	names := map[string]string{}
	for _, sid := range l {
		_, rows, err := s.read(sid)
		if err != nil {
			return t, err
		}
		skip := s.metas[sid].carried
		started := false
		for _, r := range rows {
			if !started && r.Role == "system" && s.metas[sid].Parent != "" {
				continue // the compaction summary
			}
			started = true
			if skip > 0 {
				skip--
				continue
			}
			for _, c := range r.ToolCalls {
				names[c.ID] = c.Name
			}
			if r.Role == "tool" {
				r.ToolName = names[r.ToolCallID]
			}
			t.Data = append(t.Data, r)
		}
	}
	if limit > 0 && len(t.Data) > limit {
		if latest {
			t.Data = t.Data[len(t.Data)-limit:]
		} else {
			t.Data = t.Data[:limit]
		}
	}
	return t, nil
}

// DeleteLineage removes every session of the conversation id belongs to; OnGone's functions are
// told.
func (s *Store) DeleteLineage(id string) (int, error) {
	s.mu.Lock()
	l, ok := s.lineage(id)
	if !ok {
		s.mu.Unlock()
		return 0, ErrNotFound
	}
	gone := s.gone
	n := 0
	var err error
	for _, sid := range l {
		if rerr := os.Remove(s.path(sid)); rerr != nil && !os.IsNotExist(rerr) {
			err = rerr
			break
		}
		delete(s.metas, sid)
		n++
	}
	if err == nil {
		err = s.writeIndex()
	}
	s.mu.Unlock()
	if n > 0 {
		s.tell(l[0], GoneDeleted, gone)
	}
	return n, err
}

// writeIndex replaces sessions/index.json: every conversation's summary.
func (s *Store) writeIndex() error {
	body, err := json.Marshal(struct {
		V        int                   `json:"v"`
		Sessions []wire.SessionSummary `json:"sessions"`
	}{wire.SessionVersion, s.summaries()})
	if err != nil {
		return err
	}
	return fsx.WriteAtomicIn(filepath.Join(s.dir, "index.json"), s.tmp, append(body, '\n'), 0o600)
}
