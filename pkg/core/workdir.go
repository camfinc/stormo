package core

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/camfinc/stormo/pkg/shared"
)

// The shared workdir (docs/core.md §4, phase 4). Locally the shared space lives in workdir/ at the
// instance root: workdir/group and workdir/<unit>, each bind-mounted into an agent as
// /shared/<layer> exactly where EFS mounts it on AWS, and only group plus the agent's own unit.
// Agents use files normally; the core adds coordination on top: who created and last changed
// each file, and locks (hard, soft, temp) that agents take and check through MCP (fs_*) or the
// workdir.py helper. Nothing is enforced on the filesystem: a write under another agent's hard
// lock is seen by the scan and becomes a finding.

// Lock kinds and their default lifetimes.
var lockTTL = map[string]time.Duration{"hard": 30 * time.Minute, "soft": 2 * time.Hour, "temp": 5 * time.Minute}

const (
	maxLockTTL = 8 * time.Hour
	// A lock whose owner has been stopped this long is released.
	lockGrace = 5 * time.Minute
	// A file tool's or fs_note's claim names the writer of a change seen this close to it.
	claimWindow = 2 * time.Minute
	maxWrite    = 1 << 20
	maxHash     = 256 << 20
)

// ShellTools are the tool calls that may write files without naming them (terminal attribution).
var ShellTools = []string{"terminal", "execute_code", "process"}

// Lock is a lock as agents and the owner see it.
type Lock struct {
	Owner   string `json:"owner"`
	Path    string `json:"path"` // as agents see it: /shared/<layer>/<pattern>
	Kind    string `json:"kind"`
	Reason  string `json:"reason,omitempty"`
	Created string `json:"created"`
	Expires string `json:"expires"`
}

// FileEvent is one change to a file.
type FileEvent struct {
	At        string `json:"at"`
	Kind      string `json:"kind"` // created | modified | deleted
	Agent     string `json:"agent"`
	How       string `json:"how"` // fs_write | tool:<name> | note | terminal (inferred) | scan
	Violation string `json:"violation,omitempty"`
}

// FileInfo is fs_info's answer.
type FileInfo struct {
	Path       string      `json:"path"`
	Exists     bool        `json:"exists"`
	Dir        bool        `json:"dir,omitempty"`
	Size       int64       `json:"size,omitempty"`
	Modified   string      `json:"modified,omitempty"`
	Creator    string      `json:"creator,omitempty"`
	LastWriter string      `json:"lastWriter,omitempty"`
	How        string      `json:"how,omitempty"`
	Locks      []Lock      `json:"locks"`
	Warnings   []string    `json:"warnings,omitempty"`
	History    []FileEvent `json:"history"`
}

// Entry is one line of fs_ls.
type Entry struct {
	Name       string `json:"name"`
	Dir        bool   `json:"dir,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Modified   string `json:"modified,omitempty"`
	LastWriter string `json:"lastWriter,omitempty"`
	Locked     string `json:"locked,omitempty"` // holder and kind of a lock covering it
}

// WorkdirOptions configure a Workdir.
type WorkdirOptions struct {
	// Root is the instance's workdir/ (shared.LocalDir).
	Root    string
	DB      *DB
	Monitor *Monitor
	// Roster gives each agent's unit and whether it runs (BusAgent).
	Roster func() []BusAgent
	Now    func() time.Time
}

type claim struct {
	agent, how string
	at         int64
}

// Workdir tracks the shared workdir; safe for concurrent use.
type Workdir struct {
	o WorkdirOptions

	mu       sync.Mutex // claims, downSince
	claims   map[string][]claim
	downAt   map[string]int64
	scanMu   sync.Mutex
	scanned  bool
	writeMus sync.Mutex
}

// NewWorkdir builds the tracker; call Run for the scan and lock sweep.
func NewWorkdir(o WorkdirOptions) *Workdir {
	if o.Now == nil {
		o.Now = time.Now
	}
	w := &Workdir{o: o, claims: map[string][]claim{}, downAt: map[string]int64{}}
	if o.Monitor != nil {
		o.Monitor.OnToolDone(func(e ToolEvent) {
			if key, ok := w.keyOf(e.Path); ok {
				w.claim(key, e.Agent, "tool:"+e.Tool, e.At)
			}
		})
	}
	return w
}

func (w *Workdir) agent(id string) (BusAgent, bool) {
	if w.o.Roster != nil {
		for _, a := range w.o.Roster() {
			if a.ID == id {
				return a, true
			}
		}
	}
	return BusAgent{}, false
}

// keyOf is the workdir key ("<layer>/<rel>") of a container path, without an access check.
func (w *Workdir) keyOf(p string) (string, bool) {
	clean := path.Clean("/" + strings.TrimPrefix(p, "/"))
	rest, ok := strings.CutPrefix(clean, shared.Root+"/")
	if !ok || rest == "" {
		return "", false
	}
	return rest, true
}

// resolve checks that agent may use p (a container path under /shared, maybe a glob) and gives
// its key and layer. allowRoot accepts the layer itself ("/shared/group").
func (w *Workdir) resolve(agent, p string, allowRoot bool) (key, layer string, err error) {
	a, ok := w.agent(agent)
	if !ok {
		return "", "", toolErr("the core does not know agent %s", agent)
	}
	if !strings.HasPrefix(p, "/") {
		return "", "", toolErr("%q: give the absolute path (/shared/group/… or /shared/%s/…)", p, a.Unit)
	}
	if strings.ContainsRune(p, 0) {
		return "", "", toolErr("bad path")
	}
	key, ok = w.keyOf(p)
	if !ok {
		return "", "", toolErr("%q is not in the shared space (/shared/group/… or /shared/%s/…)", p, a.Unit)
	}
	layer, rel, _ := strings.Cut(key, "/")
	if layer != "group" && layer != a.Unit {
		return "", "", toolErr("%q: /shared/%s is not mounted for you", p, layer)
	}
	if rel == "" && !allowRoot {
		return "", "", toolErr("%q is a whole layer: name a file, a directory or a glob in it", p)
	}
	return key, layer, nil
}

func (w *Workdir) host(key string) string { return filepath.Join(w.o.Root, filepath.FromSlash(key)) }

func shown(key string) string { return shared.Root + "/" + key }

func hasMeta(p string) bool { return strings.ContainsAny(p, "*?[{") }

// staticPrefix is the part of a pattern before its first glob segment.
func staticPrefix(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		if hasMeta(s) {
			return strings.Join(segs[:i], "/")
		}
	}
	return p
}

// covers: a lock on pattern covers key (the path itself, anything under it, or a glob match).
func covers(pattern, key string) bool {
	if pattern == key || strings.HasPrefix(key, pattern+"/") {
		return true
	}
	if hasMeta(pattern) {
		if ok, _ := doublestar.Match(pattern, key); ok {
			return true
		}
		// A glob on a directory covers what is under its matches.
		for d := path.Dir(key); d != "." && d != "/"; d = path.Dir(d) {
			if ok, _ := doublestar.Match(pattern, d); ok {
				return true
			}
		}
	}
	return false
}

// overlap: two lock patterns can name a common file.
func overlap(a, b string) bool {
	if covers(a, b) || covers(b, a) {
		return true
	}
	if hasMeta(a) && hasMeta(b) {
		pa, pb := staticPrefix(a), staticPrefix(b)
		return pa == pb || strings.HasPrefix(pa, pb+"/") || strings.HasPrefix(pb, pa+"/")
	}
	return false
}

type lockRow struct {
	id                   int64
	owner, pattern, kind string
	reason               string
	created, expires     int64
}

func (r lockRow) view() Lock {
	return Lock{Owner: r.owner, Path: shown(r.pattern), Kind: r.kind, Reason: r.reason, Created: isoMs(r.created), Expires: isoMs(r.expires)}
}

func (w *Workdir) locks(where string, args ...any) ([]lockRow, error) {
	rows, err := w.o.DB.Query(`SELECT id, owner, pattern, kind, reason, created, expires FROM locks WHERE expires > ?`+where+` ORDER BY created`,
		append([]any{w.o.Now().UnixMilli()}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []lockRow{}
	for rows.Next() {
		var r lockRow
		if err := rows.Scan(&r.id, &r.owner, &r.pattern, &r.kind, &r.reason, &r.created, &r.expires); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func describe(r lockRow) string {
	s := fmt.Sprintf("%s holds a %s lock on %s until %s", r.owner, r.kind, shown(r.pattern), isoMs(r.expires))
	if r.reason != "" {
		s += " (" + r.reason + ")"
	}
	return s
}

// LockResult is fs_lock's answer.
type LockResult struct {
	Lock     Lock     `json:"lock"`
	Warnings []string `json:"warnings,omitempty"`
}

// Lock takes (or refreshes) agent's lock on p.
func (w *Workdir) Lock(agent, p, kind, reason string, minutes int) (*LockResult, error) {
	if kind == "" {
		kind = "hard"
	}
	ttl, ok := lockTTL[kind]
	if !ok {
		return nil, toolErr(`kind is "hard" (exclusive: I am writing this), "soft" (advisory: coordinate first) or "temp" (a short lease for one operation)`)
	}
	if minutes > 0 && kind != "temp" {
		ttl = min(time.Duration(minutes)*time.Minute, maxLockTTL)
	}
	key, _, err := w.resolve(agent, p, false)
	if err != nil {
		return nil, err
	}
	if hasMeta(key) && !doublestar.ValidatePattern(key) {
		return nil, toolErr("%q is not a valid glob", p)
	}
	held, err := w.locks("")
	if err != nil {
		return nil, err
	}
	res := &LockResult{}
	for _, r := range held {
		if r.owner == agent || !overlap(r.pattern, key) {
			continue
		}
		exclusive := r.kind == "hard" || r.kind == "temp"
		if exclusive && kind != "soft" {
			return nil, toolErr("locked: %s. Wait for it, or ask %s with msg_send", describe(r), r.owner)
		}
		res.Warnings = append(res.Warnings, describe(r))
	}
	now := w.o.Now().UnixMilli()
	reason = clip(reason, 200)
	if _, err := w.o.DB.Exec(`INSERT INTO locks(owner, pattern, kind, reason, created, expires) VALUES(?,?,?,?,?,?)
		ON CONFLICT(owner, pattern) DO UPDATE SET kind = excluded.kind, reason = excluded.reason, expires = excluded.expires`,
		agent, key, kind, reason, now, now+ttl.Milliseconds()); err != nil {
		return nil, err
	}
	mine, err := w.locks(` AND owner = ? AND pattern = ?`, agent, key)
	if err != nil || len(mine) == 0 {
		return nil, errors.Join(err, errors.New("lock not stored"))
	}
	res.Lock = mine[0].view()
	return res, nil
}

// Unlock releases agent's lock on p (the same path or glob it locked).
func (w *Workdir) Unlock(agent, p string) error {
	key, _, err := w.resolve(agent, p, false)
	if err != nil {
		return err
	}
	res, err := w.o.DB.Exec(`DELETE FROM locks WHERE owner = ? AND pattern = ?`, agent, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return toolErr("you hold no lock on %s (fs_locks lists yours)", shown(key))
	}
	return nil
}

// Renew extends agent's hard or soft lock on p by its kind's lifetime (or minutes).
func (w *Workdir) Renew(agent, p string, minutes int) (*Lock, error) {
	key, _, err := w.resolve(agent, p, false)
	if err != nil {
		return nil, err
	}
	mine, err := w.locks(` AND owner = ? AND pattern = ?`, agent, key)
	if err != nil {
		return nil, err
	}
	if len(mine) == 0 {
		return nil, toolErr("you hold no lock on %s", shown(key))
	}
	r := mine[0]
	if r.kind == "temp" {
		return nil, toolErr("a temp lock is never renewed: take a hard lock if the work takes longer")
	}
	ttl := lockTTL[r.kind]
	if minutes > 0 {
		ttl = min(time.Duration(minutes)*time.Minute, maxLockTTL)
	}
	r.expires = w.o.Now().Add(ttl).UnixMilli()
	if _, err := w.o.DB.Exec(`UPDATE locks SET expires = ? WHERE id = ?`, r.expires, r.id); err != nil {
		return nil, err
	}
	v := r.view()
	return &v, nil
}

// visible keeps the locks in layers agent mounts.
func (w *Workdir) visible(agent string, rows []lockRow) []Lock {
	a, _ := w.agent(agent)
	out := []Lock{}
	for _, r := range rows {
		layer, _, _ := strings.Cut(r.pattern, "/")
		if layer == "group" || layer == a.Unit {
			out = append(out, r.view())
		}
	}
	return out
}

// Locks are the live locks agent can see, under p when given.
func (w *Workdir) Locks(agent, p string) ([]Lock, error) {
	rows, err := w.locks("")
	if err != nil {
		return nil, err
	}
	if p != "" {
		key, _, err := w.resolve(agent, p, true)
		if err != nil {
			return nil, err
		}
		rows = slices.DeleteFunc(rows, func(r lockRow) bool { return !overlap(r.pattern, key) && !covers(key, r.pattern) })
	}
	return w.visible(agent, rows), nil
}

// covering are the locks on key held by someone other than agent.
func (w *Workdir) covering(key, agent string) ([]lockRow, error) {
	rows, err := w.locks("")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(rows, func(r lockRow) bool { return r.owner == agent || !covers(r.pattern, key) }), nil
}

func (w *Workdir) history(key string, n int) ([]FileEvent, error) {
	rows, err := w.o.DB.Query(`SELECT at, kind, agent, how, violation FROM file_events WHERE path = ? ORDER BY at DESC, id DESC LIMIT ?`, key, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileEvent{}
	for rows.Next() {
		var e FileEvent
		var at int64
		if err := rows.Scan(&at, &e.Kind, &e.Agent, &e.How, &e.Violation); err != nil {
			return nil, err
		}
		e.At = isoMs(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Info is what the core knows about p: the file, who created and last changed it, its locks.
func (w *Workdir) Info(agent, p string) (*FileInfo, error) {
	key, _, err := w.resolve(agent, p, true)
	if err != nil {
		return nil, err
	}
	info := &FileInfo{Path: shown(key), Locks: []Lock{}}
	if st, err := os.Stat(w.host(key)); err == nil {
		info.Exists, info.Dir = true, st.IsDir()
		if !st.IsDir() {
			info.Size, info.Modified = st.Size(), isoMs(st.ModTime().UnixMilli())
		}
	}
	var created, changed int64
	err = w.o.DB.QueryRow(`SELECT creator, writer, how, created, changed FROM files WHERE path = ?`, key).Scan(&info.Creator, &info.LastWriter, &info.How, &created, &changed)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := w.locks("")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if covers(r.pattern, key) || (info.Dir && covers(key, r.pattern)) {
			info.Locks = append(info.Locks, r.view())
			if r.owner != agent {
				info.Warnings = append(info.Warnings, describe(r))
			}
		}
	}
	if info.History, err = w.history(key, 10); err != nil {
		return nil, err
	}
	return info, nil
}

// Ls lists a directory of the shared space with each entry's last writer and lock.
func (w *Workdir) Ls(agent, p string) ([]Entry, error) {
	key, _, err := w.resolve(agent, p, true)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(w.host(key))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, toolErr("%s does not exist", shown(key))
		}
		return nil, toolErr("%s is not a directory", shown(key))
	}
	held, err := w.locks("")
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, d := range des {
		if d.Type()&fs.ModeSymlink != 0 {
			continue
		}
		k := key + "/" + d.Name()
		e := Entry{Name: d.Name(), Dir: d.IsDir()}
		if st, err := d.Info(); err == nil && !d.IsDir() {
			e.Size, e.Modified = st.Size(), isoMs(st.ModTime().UnixMilli())
		}
		_ = w.o.DB.QueryRow(`SELECT writer FROM files WHERE path = ?`, k).Scan(&e.LastWriter)
		for _, r := range held {
			if covers(r.pattern, k) {
				e.Locked = r.owner + " (" + r.kind + ")"
				break
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// WriteResult is fs_write's answer.
type WriteResult struct {
	Path     string   `json:"path"`
	Size     int      `json:"size"`
	Created  bool     `json:"created"`
	Warnings []string `json:"warnings,omitempty"`
}

// Write writes content to p for agent, refused under another agent's hard or temp lock, and
// attributed exactly. mode is overwrite (default), create (fails if it exists) or append.
func (w *Workdir) Write(agent, p, content, mode string) (*WriteResult, error) {
	key, _, err := w.resolve(agent, p, false)
	if err != nil {
		return nil, err
	}
	if hasMeta(key) {
		return nil, toolErr("write one file, not a glob")
	}
	if len(content) > maxWrite {
		return nil, toolErr("at most %d bytes through fs_write; write larger files with your file tools, then fs_note", maxWrite)
	}
	if mode == "" {
		mode = "overwrite"
	}
	if mode != "overwrite" && mode != "create" && mode != "append" {
		return nil, toolErr(`mode is "overwrite", "create" or "append"`)
	}
	w.writeMus.Lock()
	defer w.writeMus.Unlock()
	locks, err := w.covering(key, agent)
	if err != nil {
		return nil, err
	}
	res := &WriteResult{Path: shown(key), Size: len(content)}
	for _, r := range locks {
		if r.kind == "hard" || r.kind == "temp" {
			return nil, toolErr("not written: %s. Wait for it, or ask %s with msg_send", describe(r), r.owner)
		}
		res.Warnings = append(res.Warnings, describe(r))
	}
	file := w.host(key)
	st, statErr := os.Stat(file)
	exists := statErr == nil
	if exists && st.IsDir() {
		return nil, toolErr("%s is a directory", shown(key))
	}
	if exists && mode == "create" {
		return nil, toolErr("%s already exists (mode create)", shown(key))
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o775); err != nil {
		return nil, err
	}
	data := []byte(content)
	if mode == "append" && exists {
		old, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		data = append(old, data...)
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".fs_write-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Chmod(0o664); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return nil, err
	}
	res.Created = !exists
	kind := "modified"
	if !exists {
		kind = "created"
	}
	if err := w.record(key, file, kind, agent, "fs_write", "", w.o.Now().UnixMilli()); err != nil {
		return nil, err
	}
	return res, nil
}

// Note records that agent changed p (after a terminal write) so the change is attributed to it.
func (w *Workdir) Note(agent, p, action string) error {
	key, _, err := w.resolve(agent, p, false)
	if err != nil {
		return err
	}
	if action == "" {
		action = "modified"
	}
	if action != "created" && action != "modified" && action != "deleted" {
		return toolErr(`action is "created", "modified" or "deleted"`)
	}
	now := w.o.Now().UnixMilli()
	w.claim(key, agent, "note", now)
	// The scan may have seen the change first and not known who made it: the path's newest event,
	// if nobody is named on it (events carry the file's mtime, which can be older than the note).
	res, err := w.o.DB.Exec(`UPDATE file_events SET agent = ?, how = 'note' WHERE id = (SELECT id FROM file_events WHERE path = ? ORDER BY at DESC, id DESC LIMIT 1)
		AND agent = 'unknown' AND at >= ?`, agent, key, now-(24*time.Hour).Milliseconds())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		_, err = w.o.DB.Exec(`UPDATE files SET writer = ?, how = 'note', creator = CASE WHEN creator = 'unknown' THEN ? ELSE creator END WHERE path = ?`, agent, agent, key)
	}
	return err
}

func (w *Workdir) claim(key, agent, how string, at int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.claims[key] = append(w.claims[key], claim{agent, how, at})
}

// takeClaim is the claim for a change of key at mtime, removing it.
func (w *Workdir) takeClaim(key string, mtime int64) (claim, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cs := w.claims[key]
	for i := len(cs) - 1; i >= 0; i-- {
		if d := cs[i].at - mtime; d > -claimWindow.Milliseconds() && d < claimWindow.Milliseconds() {
			c := cs[i]
			w.claims[key] = append(cs[:i:i], cs[i+1:]...)
			return c, true
		}
	}
	return claim{}, false
}

// shellAt is the one agent with access to layer that had a shell tool call running at ms, if
// exactly one did (the activity timeline holds the calls; delivery lag allowed for).
func (w *Workdir) shellAt(layer string, ms int64) string {
	if w.o.Monitor == nil {
		return ""
	}
	q := `SELECT agent, call, event, at FROM activity WHERE tool IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ShellTools)), ",") + `)
		AND at BETWEEN ? AND ? ORDER BY at`
	args := []any{}
	for _, t := range ShellTools {
		args = append(args, t)
	}
	args = append(args, ms-callTimeout.Milliseconds(), ms+10_000)
	rows, err := w.o.DB.Query(q, args...)
	if err != nil {
		return ""
	}
	defer rows.Close()
	type span struct{ start, end int64 }
	calls := map[string]*span{}
	agentOf := map[string]string{}
	for rows.Next() {
		var agent, call, event string
		var at int64
		if rows.Scan(&agent, &call, &event, &at) != nil {
			return ""
		}
		k := agent + "\x00" + call
		agentOf[k] = agent
		s := calls[k]
		if s == nil {
			s = &span{start: -1, end: -1}
			calls[k] = s
		}
		if event == "pre_tool_call" {
			s.start = at
		} else if event == "post_tool_call" {
			s.end = at
		}
	}
	found := map[string]bool{}
	for k, s := range calls {
		// Running at ms: started by then (2 s slack) and not finished before it (2 s slack).
		if s.start >= 0 && s.start <= ms+2_000 && (s.end < 0 || s.end >= ms-2_000) {
			found[agentOf[k]] = true
		}
	}
	cands := []string{}
	for a := range found {
		if ag, ok := w.agent(a); ok && (layer == "group" || ag.Unit == layer) {
			cands = append(cands, a)
		}
	}
	if len(cands) == 1 {
		return cands[0]
	}
	return ""
}

// record stores one change: the file row and the event, with a lock check.
func (w *Workdir) record(key, file, kind, agent, how, sha string, at int64) error {
	violation := ""
	if locks, err := w.covering(key, agent); err == nil {
		for _, r := range locks {
			if (r.kind == "hard" || r.kind == "temp") && r.created <= at {
				violation = r.owner
				who := agent
				if who == "" {
					who = "unknown"
				}
				if err := w.o.DB.RaiseFinding(who, "lock_violation", "high", fmt.Sprintf("%s %s under %s's %s lock", shown(key), kind, r.owner, r.kind), at); err != nil {
					log.Printf("workdir: finding: %v", err)
				}
				break
			}
		}
	}
	if _, err := w.o.DB.Exec(`INSERT INTO file_events(path, at, kind, agent, how, violation) VALUES(?,?,?,?,?,?)`, key, at, kind, agent, how, violation); err != nil {
		return err
	}
	if kind == "deleted" {
		_, err := w.o.DB.Exec(`DELETE FROM files WHERE path = ?`, key)
		return err
	}
	st, err := os.Stat(file)
	if err != nil {
		return nil // gone again before we looked
	}
	if sha == "" {
		sha = hashFile(file, st.Size())
	}
	_, err = w.o.DB.Exec(`INSERT INTO files(path, size, mtime, sha256, creator, created, writer, how, changed) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET size = excluded.size, mtime = excluded.mtime, sha256 = excluded.sha256, writer = excluded.writer,
		how = excluded.how, changed = excluded.changed`, key, st.Size(), st.ModTime().UnixMilli(), sha, agent, at, agent, how, at)
	return err
}

func hashFile(file string, size int64) string {
	if size > maxHash {
		return ""
	}
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

type known struct {
	size, mtime int64
	sha         string
}

// Scan walks workdir/ and records what changed since the last scan. The first scan of an empty
// table only takes stock (no events: nothing to compare with).
func (w *Workdir) Scan() error {
	w.scanMu.Lock()
	defer w.scanMu.Unlock()
	prev := map[string]known{}
	rows, err := w.o.DB.Query(`SELECT path, size, mtime, sha256 FROM files`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k string
		var f known
		if err := rows.Scan(&k, &f.size, &f.mtime, &f.sha); err != nil {
			rows.Close()
			return err
		}
		prev[k] = f
	}
	rows.Close()
	stock := !w.scanned && len(prev) == 0
	w.scanned = true
	now := w.o.Now().UnixMilli()
	seen := map[string]bool{}
	err = filepath.WalkDir(w.o.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == w.o.Root && errors.Is(err, fs.ErrNotExist) {
				return filepath.SkipAll
			}
			return nil // unreadable entry: skip it
		}
		if d.IsDir() || !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".fs_write-") {
			return nil
		}
		rel, err := filepath.Rel(w.o.Root, p)
		if err != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if !strings.Contains(key, "/") {
			return nil // a file loose in workdir/ is in no layer
		}
		st, err := d.Info()
		if err != nil {
			return nil
		}
		seen[key] = true
		mtime := st.ModTime().UnixMilli()
		old, had := prev[key]
		if had && old.size == st.Size() && old.mtime == mtime {
			return nil
		}
		sha := hashFile(p, st.Size())
		if had && sha != "" && sha == old.sha {
			_, _ = w.o.DB.Exec(`UPDATE files SET mtime = ? WHERE path = ?`, mtime, key) // touched, not changed
			return nil
		}
		if stock {
			_, _ = w.o.DB.Exec(`INSERT OR IGNORE INTO files(path, size, mtime, sha256, created, changed) VALUES(?,?,?,?,?,?)`, key, st.Size(), mtime, sha, mtime, mtime)
			return nil
		}
		kind := "modified"
		if !had {
			kind = "created"
		}
		agent, how := w.attribute(key, mtime)
		if err := w.record(key, p, kind, agent, how, sha, mtime); err != nil {
			log.Printf("workdir: %s: %v", key, err)
		}
		if !had && agent != "unknown" {
			_, _ = w.o.DB.Exec(`UPDATE files SET creator = ? WHERE path = ?`, agent, key)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for key := range prev {
		if !seen[key] {
			agent, how := w.attribute(key, now)
			if err := w.record(key, w.host(key), "deleted", agent, how, "", now); err != nil {
				log.Printf("workdir: %s: %v", key, err)
			}
		}
	}
	return nil
}

// attribute names who changed key at ms: a claim (file tool or fs_note), else the one agent
// running a shell tool then, else unknown.
func (w *Workdir) attribute(key string, ms int64) (agent, how string) {
	if c, ok := w.takeClaim(key, ms); ok {
		return c.agent, c.how
	}
	layer, _, _ := strings.Cut(key, "/")
	if a := w.shellAt(layer, ms); a != "" {
		return a, "terminal (inferred)"
	}
	return "unknown", "scan"
}

// Sweep drops expired locks and the locks of agents stopped for longer than the grace period.
func (w *Workdir) Sweep() error {
	now := w.o.Now().UnixMilli()
	if _, err := w.o.DB.Exec(`DELETE FROM locks WHERE expires <= ?`, now); err != nil {
		return err
	}
	if w.o.Roster == nil {
		return nil
	}
	w.mu.Lock()
	gone := []string{}
	for _, a := range w.o.Roster() {
		if a.Running {
			delete(w.downAt, a.ID)
			continue
		}
		if _, ok := w.downAt[a.ID]; !ok {
			w.downAt[a.ID] = now
		}
		if now-w.downAt[a.ID] >= lockGrace.Milliseconds() {
			gone = append(gone, a.ID)
		}
	}
	w.mu.Unlock()
	for _, a := range gone {
		if _, err := w.o.DB.Exec(`DELETE FROM locks WHERE owner = ?`, a); err != nil {
			return err
		}
	}
	// Claims nobody used.
	w.mu.Lock()
	for k, cs := range w.claims {
		cs = slices.DeleteFunc(cs, func(c claim) bool { return now-c.at > 2*claimWindow.Milliseconds() })
		if len(cs) == 0 {
			delete(w.claims, k)
		} else {
			w.claims[k] = cs
		}
	}
	w.mu.Unlock()
	return nil
}

// Run scans and sweeps every interval until stop closes.
func (w *Workdir) Run(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := w.Scan(); err != nil {
			log.Printf("workdir scan: %v", err)
		}
		if err := w.Sweep(); err != nil {
			log.Printf("workdir sweep: %v", err)
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// LockCounts are the live locks per owner (the office shows counts, not paths).
func (w *Workdir) LockCounts() map[string]int {
	out := map[string]int{}
	rows, err := w.locks("")
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.owner]++
	}
	return out
}

// OwnerView is the owner's look at the workdir: every live lock and the newest changes.
type OwnerView struct {
	Locks   []Lock         `json:"locks"`
	Changes []OwnerChange  `json:"changes"`
	Layers  map[string]int `json:"layers"` // files per layer
}

// OwnerChange is one change with its path.
type OwnerChange struct {
	Path string `json:"path"`
	FileEvent
}

// Owner is the workdir as its owner sees it.
func (w *Workdir) Owner(limit int) (*OwnerView, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := w.locks("")
	if err != nil {
		return nil, err
	}
	v := &OwnerView{Locks: []Lock{}, Changes: []OwnerChange{}, Layers: map[string]int{}}
	for _, r := range rows {
		v.Locks = append(v.Locks, r.view())
	}
	evs, err := w.o.DB.Query(`SELECT path, at, kind, agent, how, violation FROM file_events ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer evs.Close()
	for evs.Next() {
		var c OwnerChange
		var at int64
		if err := evs.Scan(&c.Path, &at, &c.Kind, &c.Agent, &c.How, &c.Violation); err != nil {
			return nil, err
		}
		c.Path, c.At = shown(c.Path), isoMs(at)
		v.Changes = append(v.Changes, c)
	}
	files, err := w.o.DB.Query(`SELECT path FROM files`)
	if err != nil {
		return nil, err
	}
	defer files.Close()
	for files.Next() {
		var k string
		if err := files.Scan(&k); err != nil {
			return nil, err
		}
		layer, _, _ := strings.Cut(k, "/")
		v.Layers[layer]++
	}
	return v, nil
}

// WorkdirTools are the fs_* MCP tools.
func WorkdirTools(w *Workdir) []MCPTool {
	pathArg := str("an absolute path in the shared space: /shared/group/… or /shared/<your unit>/…")
	type pathOnly struct {
		Path string `json:"path"`
	}
	one := func(c ToolCall) (string, error) {
		var a pathOnly
		if err := decodeArgs(c, &a); err != nil {
			return "", err
		}
		return a.Path, nil
	}
	return []MCPTool{
		{
			Name: "fs_lock",
			Description: "Lock a file, a directory or a glob in the shared space before working on it, so other agents see it is " +
				"taken. kind: hard (exclusive, I am writing this; 30 min, renewable), soft (advisory, I am working on it, " +
				"coordinate first; 2 h), temp (a 5-minute lease for one operation). Refused when another agent holds a hard " +
				"or temp lock on it (the answer names the holder). Release with fs_unlock when done.",
			InputSchema: schema([]string{"path"}, map[string]any{
				"path":    pathArg,
				"kind":    map[string]any{"type": "string", "enum": []string{"hard", "soft", "temp"}},
				"reason":  str("what you are doing, for the others"),
				"minutes": integer("lifetime for a hard or soft lock (default 30 / 120, up to 480)"),
			}),
			Run: func(c ToolCall) (any, error) {
				var a struct {
					Path, Kind, Reason string
					Minutes            int
				}
				if err := decodeArgs(c, &a); err != nil {
					return nil, err
				}
				return w.Lock(c.Agent, a.Path, a.Kind, a.Reason, a.Minutes)
			},
		},
		{
			Name:        "fs_unlock",
			Description: "Release your lock on a path (the same path or glob you locked).",
			InputSchema: schema([]string{"path"}, map[string]any{"path": pathArg}),
			Run: func(c ToolCall) (any, error) {
				p, err := one(c)
				if err != nil {
					return nil, err
				}
				if err := w.Unlock(c.Agent, p); err != nil {
					return nil, err
				}
				return map[string]any{"released": p}, nil
			},
		},
		{
			Name:        "fs_renew",
			Description: "Extend your hard or soft lock on a path.",
			InputSchema: schema([]string{"path"}, map[string]any{"path": pathArg, "minutes": integer("new lifetime from now")}),
			Run: func(c ToolCall) (any, error) {
				var a struct {
					Path    string
					Minutes int
				}
				if err := decodeArgs(c, &a); err != nil {
					return nil, err
				}
				return w.Renew(c.Agent, a.Path, a.Minutes)
			},
		},
		{
			Name:        "fs_locks",
			Description: "The live locks in the shared space you can see, or only those touching path.",
			InputSchema: schema(nil, map[string]any{"path": pathArg}),
			Run: func(c ToolCall) (any, error) {
				var a struct{ Path string }
				if err := decodeArgs(c, &a); err != nil {
					return nil, err
				}
				ls, err := w.Locks(c.Agent, a.Path)
				if err != nil {
					return nil, err
				}
				return map[string]any{"locks": ls}, nil
			},
		},
		{
			Name:        "fs_info",
			Description: "What the core knows about a shared-space path: who created it and changed it last (and how that is known), its recent history, and the locks on it. Check it before changing a file another agent may be working on.",
			InputSchema: schema([]string{"path"}, map[string]any{"path": pathArg}),
			Run: func(c ToolCall) (any, error) {
				p, err := one(c)
				if err != nil {
					return nil, err
				}
				return w.Info(c.Agent, p)
			},
		},
		{
			Name:        "fs_ls",
			Description: "List a shared-space directory with each entry's last writer and lock holder.",
			InputSchema: schema([]string{"path"}, map[string]any{"path": pathArg}),
			Run: func(c ToolCall) (any, error) {
				p, err := one(c)
				if err != nil {
					return nil, err
				}
				es, err := w.Ls(c.Agent, p)
				if err != nil {
					return nil, err
				}
				return map[string]any{"entries": es}, nil
			},
		},
		{
			Name: "fs_write",
			Description: "Write a text file in the shared space through the core: refused while another agent holds a hard or " +
				"temp lock on it, and recorded as yours exactly. mode: overwrite (default), create (only if new) or append. " +
				"Up to 1 MB. Same rules as the shared space: no credentials, no client personal data in /shared/group.",
			InputSchema: schema([]string{"path", "content"}, map[string]any{
				"path":    pathArg,
				"content": str("the text to write"),
				"mode":    map[string]any{"type": "string", "enum": []string{"overwrite", "create", "append"}},
			}),
			Run: func(c ToolCall) (any, error) {
				var a struct{ Path, Content, Mode string }
				if err := decodeArgs(c, &a); err != nil {
					return nil, err
				}
				return w.Write(c.Agent, a.Path, a.Content, a.Mode)
			},
		},
		{
			Name:        "fs_note",
			Description: "Declare that you changed a shared-space file another way (a terminal command, a script), so the change is recorded as yours.",
			InputSchema: schema([]string{"path"}, map[string]any{"path": pathArg, "action": map[string]any{"type": "string", "enum": []string{"created", "modified", "deleted"}}}),
			Run: func(c ToolCall) (any, error) {
				var a struct{ Path, Action string }
				if err := decodeArgs(c, &a); err != nil {
					return nil, err
				}
				if err := w.Note(c.Agent, a.Path, a.Action); err != nil {
					return nil, err
				}
				return map[string]any{"noted": a.Path}, nil
			},
		},
	}
}
