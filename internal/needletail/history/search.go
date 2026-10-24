package history

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Bounds of what a search, a scroll and a read show.
const (
	DefaultLimit = 3
	MaxLimit     = 10
	// DefaultWindow and MaxWindow are a scroll's messages on each side of its anchor.
	DefaultWindow = 5
	MaxWindow     = 20
	// hitWindow is the messages on each side of the best hit's anchor.
	hitWindow = 3
	// message contents: the best hit's window, the other hits' anchors, a scroll's or a read's
	hitContent    = 1000
	anchorContent = 600
	scrollContent = 800
	toolContent   = 400
	// readHead and readTail are what a read of a long session shows.
	readHead, readTail = 10, 10
	// candidates are the sessions read again to rank their messages, at most.
	candidates = 30
	// maxExclude is how many exclude_session_ids count.
	maxExclude = 20
)

// Visible says whether the caller may see a conversation (its root session, and its source).
type Visible func(root, source string) bool

// Query is a search.
type Query struct {
	// Text is words (ORed; "a phrase" must appear as written, word* is a prefix).
	Text string
	// Limit is the conversations answered (DefaultLimit; at most MaxLimit).
	Limit int
	// Sort: "" by relevance (cron runs after the rest), "newest" or "oldest" by the session's
	// start.
	Sort string
	// After and Before bound the session's start: an ISO date or time, or a duration back from
	// now (7d, 24h, 2w).
	After, Before string
	// Source keeps the conversations of one source (api_server, slack, cron, cli).
	Source string
	// Roles is the rows searched: user, assistant or both (default), comma-separated.
	Roles string
	// Exclude are sessions already seen, with their conversations.
	Exclude []string
	// Current is the caller's own session (its conversation's tip): already in its context,
	// never a hit (its conversation's earlier, compacted sessions are).
	Current string
	Visible Visible
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

// Result is a search's answer.
type Result struct {
	Query   string `json:"query"`
	Count   int    `json:"count"`
	Results []Hit  `json:"results"`
	// IndexedSince, when set: sessions last written before it are not searched (the index's
	// bound).
	IndexedSince string `json:"indexed_since,omitempty"`
	Message      string `json:"message,omitempty"`
}

// Hit is a conversation's best match.
type Hit struct {
	SessionID string `json:"session_id"`
	// Conversation is the conversation's first session, when the hit is in a later one.
	Conversation   string `json:"conversation,omitempty"`
	When           string `json:"when"`
	Source         string `json:"source,omitempty"`
	Title          string `json:"title,omitempty"`
	MatchedRole    string `json:"matched_role"`
	MatchMessageID int    `json:"match_message_id"`
	Snippet        string `json:"snippet"`
	Messages       []Msg  `json:"messages"`
	MessagesBefore int    `json:"messages_before"`
	MessagesAfter  int    `json:"messages_after"`
}

// Msg is a message as a hit, a scroll or a read shows it.
type Msg struct {
	ID         int      `json:"id"`
	Role       string   `json:"role"`
	Content    string   `json:"content"`
	Timestamp  string   `json:"timestamp,omitempty"`
	From       string   `json:"from,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Overheard  bool     `json:"overheard,omitempty"`
	Unverified bool     `json:"unverified,omitempty"`
	ToolCalls  []string `json:"tool_calls,omitempty"`
	ToolName   string   `json:"tool_name,omitempty"`
	Anchor     bool     `json:"anchor,omitempty"`
	Cut        bool     `json:"content_truncated,omitempty"`
}

// errors
var (
	ErrNotFound = errors.New("no such session (session_search with a query, or with no arguments, lists the ones you can read)")
	ErrLive     = errors.New("that session is the one you are in: its messages are already in your context")
)

var relative = regexp.MustCompile(`^(\d+)\s*([hdw])$`)

// bound is a time bound as Hermes' session_search reads one.
func bound(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	if m := relative.FindStringSubmatch(strings.ToLower(s)); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := map[string]time.Duration{"h": time.Hour, "d": 24 * time.Hour, "w": 7 * 24 * time.Hour}[m[2]]
		return now.Add(-time.Duration(n) * unit), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", time.DateOnly} {
		if t, err := time.Parse(layout, strings.Replace(s, "Z", "+00:00", 1)); err == nil {
			return t, nil
		}
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("time bound %q: an ISO date or time (2026-07-01, 2026-07-01T09:00:00Z) or a duration back from now (7d, 24h, 2w)", s)
}

func roles(s string) (user, asst bool, err error) {
	if strings.TrimSpace(s) == "" {
		return true, true, nil
	}
	for _, r := range strings.Split(s, ",") {
		switch strings.TrimSpace(strings.ToLower(r)) {
		case "user":
			user = true
		case "assistant":
			asst = true
		case "":
		case "tool":
			return false, false, errors.New("tool output is not indexed: search user and assistant messages (scroll around a hit shows the tool results near it)")
		default:
			return false, false, fmt.Errorf("role_filter %q: user, assistant or both", r)
		}
	}
	return user, asst, nil
}

// cand is a session the index ranked, before its messages are read.
type cand struct {
	n       int32
	score   float64
	created time.Time
	cron    bool
}

// Search finds the conversations whose messages hold the query's words.
func (x *Index) Search(q Query) (Result, error) {
	now := time.Now()
	if q.Now != nil {
		now = q.Now()
	}
	after, err := bound(q.After, now)
	if err != nil {
		return Result{}, err
	}
	before, err := bound(q.Before, now)
	if err != nil {
		return Result{}, err
	}
	user, asst, err := roles(q.Roles)
	if err != nil {
		return Result{}, err
	}
	switch q.Sort {
	case "", "newest", "oldest":
	default:
		return Result{}, fmt.Errorf("sort %q: newest, oldest or none (relevance)", q.Sort)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	terms, phrases := memory.QueryTerms(q.Text)
	res := Result{Query: q.Text, Results: []Hit{}}
	if len(terms) == 0 {
		res.Message = "the query has no words to search for (words of two letters or more, not only stop words)"
		return res, nil
	}

	x.mu.Lock()
	defer x.mu.Unlock()
	x.refresh()
	if !x.floor.IsZero() {
		res.IndexedSince = x.floor.UTC().Format(time.RFC3339)
	}
	scores, idf := x.score(terms, user, asst)
	excluded := map[string]bool{}
	for i, id := range q.Exclude {
		if i >= maxExclude {
			break
		}
		excluded[id] = true
		excluded[x.root(id)] = true
	}
	visible := map[string]bool{}
	var cs []cand
	for n, sc := range scores {
		d := x.docs[n]
		if d.ID == q.Current {
			continue
		}
		root := x.root(d.ID)
		if excluded[d.ID] || excluded[root] {
			continue
		}
		src := x.source(d.ID)
		if q.Source != "" && src != q.Source {
			continue
		}
		created, _ := time.Parse(time.RFC3339Nano, d.Created)
		if !after.IsZero() && created.Before(after) || !before.IsZero() && !created.Before(before) {
			continue
		}
		v, ok := visible[root]
		if !ok {
			v = q.Visible != nil && q.Visible(root, src)
			visible[root] = v
		}
		if v {
			cs = append(cs, cand{n: n, score: sc, created: created, cron: src == "cron"})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		switch q.Sort {
		case "newest":
			if !a.created.Equal(b.created) {
				return a.created.After(b.created)
			}
		case "oldest":
			if !a.created.Equal(b.created) {
				return a.created.Before(b.created)
			}
		default:
			if a.cron != b.cron {
				return !a.cron // Hermes demotes cron runs: their words would crowd out the people's
			}
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if !a.created.Equal(b.created) {
			return a.created.After(b.created)
		}
		return x.docs[a.n].ID < x.docs[b.n].ID
	})
	// the caller's own conversation: what its live session carried over from the one before
	// (that one's last rows) is in the caller's context already
	liveParent, liveRows := "", 0
	if n, ok := x.byID[q.Current]; ok {
		liveParent, liveRows = x.docs[n].Parent, x.docs[n].carriedRows
	}
	m := newMatcher(terms)
	seen := map[string]bool{}
	read := 0
	for _, c := range cs {
		if len(res.Results) >= limit || read >= candidates {
			break
		}
		d := x.docs[c.n]
		root := x.root(d.ID)
		if seen[root] {
			continue
		}
		read++
		live := 0
		if d.ID == liveParent {
			live = liveRows
		}
		h, ok := x.hit(d, m, idf, phrases, user, asst, live, len(res.Results) == 0)
		if !ok {
			continue
		}
		seen[root] = true
		if root != d.ID {
			h.Conversation = root
		}
		h.Source = x.source(d.ID)
		res.Results = append(res.Results, h)
	}
	res.Count = len(res.Results)
	if res.Count == 0 {
		res.Message = "no matching conversation you can see: try other words (synonyms, both languages), a word* prefix, or fewer filters"
	} else {
		res.Message = "scroll around a hit: session_search with session_id and around_message_id (its match_message_id)"
	}
	return res, nil
}

// rowOf is a message row of a session file with its id (its place among the file's rows).
type rowOf struct {
	id int
	m  wire.Message
}

// rows reads a session file's message rows.
func (x *Index) rows(id string) ([]rowOf, error) {
	f, err := os.Open(filepath.Join(x.Dir, id+".jsonl"))
	if err != nil {
		return nil, ErrNotFound
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	if _, err := readLine(br); err != nil {
		return nil, ErrNotFound
	}
	var out []rowOf
	for {
		line, err := readLine(br)
		if err != nil {
			break
		}
		var m wire.Message
		if t := bytes.TrimSpace(line); len(t) == 0 || json.Unmarshal(t, &m) != nil {
			continue
		}
		out = append(out, rowOf{len(out), m})
	}
	return out, nil
}

// hit reads d's messages again and picks the best one: its snippet, and the messages around it
// (full: the window, else the anchor alone). Its last live rows (the caller's session carries
// them) are not candidates.
func (x *Index) hit(d *doc, m matcher, idf map[string]float64, phrases []string, user, asst bool, live int, full bool) (Hit, bool) {
	rows, err := x.rows(d.ID)
	if err != nil {
		return Hit{}, false
	}
	carryAt := ""
	if d.Parent != "" {
		for _, r := range rows {
			if r.m.Role == "system" {
				carryAt = r.m.At
				break
			}
		}
	}
	best, bestScore, bestText := -1, 0.0, ""
	for i, r := range rows {
		if r.m.Role == "user" && !user || r.m.Role == "assistant" && !asst {
			continue
		}
		if d.Parent != "" && carryAt != "" && r.m.At != "" && r.m.At < carryAt {
			continue
		}
		if i >= len(rows)-live {
			continue
		}
		text, ok := indexText(r.m)
		if !ok {
			continue
		}
		if s := rowScore(text, m, idf, phrases); s > bestScore {
			best, bestScore, bestText = i, s, text
		}
	}
	if best < 0 {
		return Hit{}, false
	}
	h := Hit{SessionID: d.ID, When: d.Created, Title: d.Title, MatchedRole: rows[best].m.Role, MatchMessageID: rows[best].id,
		Snippet: memory.MaskSecrets(snippet(bestText, m))}
	lo, hi := best, best
	limit := anchorContent
	if full {
		lo, hi, limit = max(0, best-hitWindow), min(len(rows)-1, best+hitWindow), hitContent
	}
	h.Messages = shape(rows, lo, hi, best, limit)
	h.MessagesBefore, h.MessagesAfter = lo, len(rows)-1-hi
	return h, true
}

// shape is rows[lo..hi] as Msgs (no summary rows), anchor marked, contents within limit.
func shape(rows []rowOf, lo, hi, anchor, limit int) []Msg {
	names := map[string]string{}
	for _, r := range rows[:min(len(rows), hi+1)] {
		for _, c := range r.m.ToolCalls {
			names[c.ID] = c.Name
		}
	}
	out := []Msg{}
	for i := lo; i <= hi && i < len(rows); i++ {
		r := rows[i]
		if r.m.Role == "system" {
			continue
		}
		msg := Msg{ID: r.id, Role: r.m.Role, Timestamp: r.m.At, Kind: r.m.Kind, Anchor: i == anchor}
		content := r.m.Content
		n := limit
		switch r.m.Role {
		case "user":
			u := cleanUser(content)
			content, msg.From, msg.Overheard, msg.Unverified = u.text, u.from, u.overheard, u.unverified
			if u.followUp && msg.Kind == "" {
				msg.Kind = wire.KindFollowUp
			}
		case "assistant":
			for _, c := range r.m.ToolCalls {
				msg.ToolCalls = append(msg.ToolCalls, c.Name)
			}
		case "tool":
			msg.ToolName = names[r.m.ToolCallID]
			n = min(n, toolContent)
		}
		msg.Content, msg.Cut = clip(memory.MaskSecrets(content), n)
		out = append(out, msg)
	}
	return out
}

// Scroll is a window of one session's messages (Around and Window), or, with no Around, the
// whole session read (its head and tail when long).
type Scroll struct {
	SessionID string
	// Around is the anchor's message id; nil reads the session.
	Around *int
	Window int
	// Current is the caller's own session: refused (it is in its context already).
	Current string
	Visible Visible
}

// ScrollResult is a scroll's or a read's answer.
type ScrollResult struct {
	Mode           string `json:"mode"`
	SessionID      string `json:"session_id"`
	Conversation   string `json:"conversation,omitempty"`
	When           string `json:"when"`
	Source         string `json:"source,omitempty"`
	Title          string `json:"title,omitempty"`
	Window         int    `json:"window,omitempty"`
	MessageCount   int    `json:"message_count"`
	Truncated      bool   `json:"truncated,omitempty"`
	Messages       []Msg  `json:"messages"`
	MessagesBefore int    `json:"messages_before"`
	MessagesAfter  int    `json:"messages_after"`
	Hint           string `json:"hint,omitempty"`
}

// Scroll answers a scroll or a read of a session the caller may see.
func (x *Index) Scroll(s Scroll) (ScrollResult, error) {
	id := strings.TrimSpace(s.SessionID)
	x.mu.Lock()
	defer x.mu.Unlock()
	x.refresh()
	hd, ok := x.heads[id] // any listed session, indexed or not
	if !ok {
		return ScrollResult{}, ErrNotFound
	}
	root, src := x.root(id), x.source(id)
	if s.Visible == nil || !s.Visible(root, src) {
		return ScrollResult{}, ErrNotFound
	}
	if id == s.Current {
		return ScrollResult{}, ErrLive
	}
	rows, err := x.rows(id)
	if err != nil {
		return ScrollResult{}, err
	}
	out := ScrollResult{SessionID: id, When: hd.created, Source: src, Title: hd.title, MessageCount: len(rows)}
	if root != id {
		out.Conversation = root
	}
	if s.Around == nil {
		out.Mode = "read"
		if len(rows) > readHead+readTail {
			out.Truncated = true
			out.Messages = append(shape(rows, 0, readHead-1, -1, scrollContent), shape(rows, len(rows)-readTail, len(rows)-1, -1, scrollContent)...)
			out.MessagesAfter = 0
			out.Hint = fmt.Sprintf("the session has %d messages: the first %d and the last %d are shown; scroll the middle with around_message_id", len(rows), readHead, readTail)
		} else {
			out.Messages = shape(rows, 0, len(rows)-1, -1, scrollContent)
		}
		return out, nil
	}
	at := *s.Around
	if at < 0 || at >= len(rows) {
		return ScrollResult{}, fmt.Errorf("around_message_id %d is not in session %s (its messages are 0 to %d)", at, id, len(rows)-1)
	}
	w := s.Window
	if w <= 0 {
		w = DefaultWindow
	}
	w = min(w, MaxWindow)
	lo, hi := max(0, at-w), min(len(rows)-1, at+w)
	out.Mode, out.Window = "scroll", w
	out.Messages = shape(rows, lo, hi, at, scrollContent)
	out.MessagesBefore, out.MessagesAfter = lo, len(rows)-1-hi
	out.Hint = "scroll on: around_message_id = the last message's id (forward) or the first's (back)"
	return out, nil
}

// Browse lists the recent conversations the caller may see.
type Browse struct {
	Limit   int
	Source  string
	Current string
	Visible Visible
}

// BrowseResult is a browse's answer.
type BrowseResult struct {
	Mode    string      `json:"mode"`
	Count   int         `json:"count"`
	Results []BrowseHit `json:"results"`
	Message string      `json:"message,omitempty"`
}

// BrowseHit is one conversation: its live session (tip) and its first (root).
type BrowseHit struct {
	SessionID    string `json:"session_id"`
	Conversation string `json:"conversation,omitempty"`
	Title        string `json:"title,omitempty"`
	Source       string `json:"source,omitempty"`
	Started      string `json:"started"`
	LastActive   string `json:"last_active,omitempty"`
	MessageCount int    `json:"message_count"`
	Preview      string `json:"preview,omitempty"`
}

// Browse answers the most recently active conversations, newest first; the caller's own is left
// out.
func (x *Index) Browse(b Browse) (BrowseResult, error) {
	limit := b.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	x.mu.Lock()
	defer x.mu.Unlock()
	x.refresh()
	type conv struct {
		root, tip string
		last      string
		count     int
	}
	parents := map[string]bool{}
	for _, n := range x.byID {
		if d := x.docs[n]; !d.dead && d.Parent != "" {
			parents[d.Parent] = true
		}
	}
	convs := map[string]*conv{}
	for id, n := range x.byID {
		d := x.docs[n]
		if d.dead {
			continue
		}
		root := x.root(id)
		c := convs[root]
		if c == nil {
			c = &conv{root: root}
			convs[root] = c
		}
		c.count += d.visible
		if !parents[id] && (c.tip == "" || d.Created > x.docs[x.byID[c.tip]].Created) {
			c.tip = id // the session no other continues: the live one
		}
		last := d.lastAt
		if last == "" {
			last = d.Created
		}
		if last > c.last {
			c.last = last
		}
	}
	current := ""
	if b.Current != "" {
		current = x.root(b.Current)
	}
	var list []*conv
	for root, c := range convs {
		src := x.source(root)
		if root == current || b.Source != "" && src != b.Source || b.Visible == nil || !b.Visible(root, src) {
			continue
		}
		list = append(list, c)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].last != list[j].last {
			return list[i].last > list[j].last
		}
		return list[i].root < list[j].root
	})
	out := BrowseResult{Mode: "browse", Results: []BrowseHit{}}
	for _, c := range list[:min(len(list), limit)] {
		if c.tip == "" {
			c.tip = c.root
		}
		h := BrowseHit{SessionID: c.tip, LastActive: c.last, MessageCount: c.count}
		if hd, ok := x.heads[c.root]; ok {
			h.Title, h.Source, h.Started = hd.title, hd.source, hd.created
		}
		if n, ok := x.byID[c.root]; ok && !x.docs[n].dead {
			h.Preview = memory.MaskSecrets(x.docs[n].preview)
		}
		if c.tip != c.root {
			h.Conversation = c.root
		}
		out.Results = append(out.Results, h)
	}
	out.Count = len(out.Results)
	out.Message = "search them with query, or read one with session_id"
	return out, nil
}
