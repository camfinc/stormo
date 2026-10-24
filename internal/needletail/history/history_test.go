package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// fixture is a sessions directory written by the real store, and an index over it.
type fixture struct {
	t     *testing.T
	dir   string
	store *sessions.Store
	ix    *Index
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sessions")
	store, err := sessions.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, dir: dir, store: store, ix: &Index{Dir: dir}}
}

// conv makes a session with rows (role: text pairs).
func (f *fixture) conv(id, source string, rows ...string) {
	f.t.Helper()
	if _, err := f.store.Create(wire.Session{ID: id, Source: source}); err != nil {
		f.t.Fatal(err)
	}
	f.add(id, rows...)
}

func (f *fixture) add(id string, rows ...string) {
	f.t.Helper()
	var ms []wire.Message
	for i := 0; i+1 < len(rows); i += 2 {
		ms = append(ms, wire.Message{Role: rows[i], Content: rows[i+1]})
	}
	if _, err := f.store.Append(id, ms...); err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.Sync(id); err != nil {
		f.t.Fatal(err)
	}
}

// all sees everything but cli sessions.
func all(root, source string) bool { return source != "cli" }

func (f *fixture) search(q Query) Result {
	f.t.Helper()
	if q.Visible == nil {
		q.Visible = all
	}
	r, err := f.ix.Search(q)
	if err != nil {
		f.t.Fatalf("search %+v: %v", q, err)
	}
	return r
}

func ids(r Result) []string {
	out := []string{}
	for _, h := range r.Results {
		out = append(out, h.SessionID)
	}
	return out
}

// A word from another conversation is found with its session, role, message id and snippet; a
// conversation the caller may not see is not; the caller's own live session never is, its
// compacted past is.
func TestSearchScopesAndLineage(t *testing.T) {
	f := newFixture(t)
	f.conv("slack-D1-dm", "slack", "user", "[Slack message from Ana (U1)]\nWe settled on the zanahoria budget for March.", "assistant", "Noted: zanahoria budget, March.")
	f.conv("slack-D2-dm", "slack", "user", "[Slack message from Bob (U2)]\nMy zanahoria order is late.", "assistant", "I will check it.")
	f.conv("api-1", "api_server", "user", "zanahoria pricing question", "assistant", "It is 3 euros.")
	r := f.search(Query{Text: "zanahoria", Visible: func(root, _ string) bool { return root == "slack-D1-dm" }, Limit: 5})
	if len(r.Results) != 1 || r.Results[0].SessionID != "slack-D1-dm" {
		t.Fatalf("scoped search = %v", ids(r))
	}
	h := r.Results[0]
	if h.MatchedRole == "" || !strings.Contains(strings.ToLower(h.Snippet), "zanahoria") || h.Source != "slack" || h.When == "" {
		t.Errorf("hit = %+v", h)
	}
	if strings.Contains(h.Snippet, "[Slack message from") {
		t.Errorf("the runtime's header is in the snippet: %q", h.Snippet)
	}
	if len(h.Messages) == 0 || h.Messages[0].From != "Ana (U1)" && h.Messages[len(h.Messages)-1].From != "Ana (U1)" {
		t.Errorf("messages = %+v", h.Messages)
	}
	// the caller's live session is already in its context
	r = f.search(Query{Text: "zanahoria", Current: "slack-D1-dm", Visible: func(root, _ string) bool { return root == "slack-D1-dm" }})
	if len(r.Results) != 0 {
		t.Errorf("found the live session: %v", ids(r))
	}
	// after a compaction its past is searchable from the new tip, as its own conversation
	tip, err := f.store.Compact("slack-D1-dm", "summary of zanahoria talk", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.add(tip, "user", "[Slack message from Ana (U1)]\nAnything else?", "assistant", "No.")
	r = f.search(Query{Text: "zanahoria", Current: tip, Visible: func(root, _ string) bool { return root == "slack-D1-dm" }})
	if got := ids(r); len(got) != 1 || got[0] != "slack-D1-dm" {
		t.Fatalf("compacted past = %v", got)
	}
	if r.Results[0].Conversation != "" {
		t.Errorf("a root's hit names its conversation: %+v", r.Results[0])
	}
	// one hit per conversation, the best of its sessions
	r = f.search(Query{Text: "zanahoria", Limit: 10})
	if got := ids(r); len(got) != 3 {
		t.Errorf("every conversation once: %v", got)
	}
}

// What the caller's live session carried over from the one before (the turn's own input among
// it, when a compaction ran at the turn's start) is in its context: not a hit.
func TestSearchSkipsWhatTheLiveSessionCarries(t *testing.T) {
	f := newFixture(t)
	f.conv("dm", "slack", "user", "old remolacha talk", "assistant", "ok", "user", "RECALL remolacha")
	rows, _ := f.store.Messages("dm")
	time.Sleep(5 * time.Millisecond) // the summary is written after the rows it follows, as a compaction's model call takes
	tip, err := f.store.Compact("dm", "summary", rows[2:])
	if err != nil {
		t.Fatal(err)
	}
	r := f.search(Query{Text: "remolacha", Current: tip})
	if len(r.Results) != 1 || r.Results[0].MatchMessageID != 0 || !strings.Contains(r.Results[0].Snippet, "old remolacha") {
		t.Errorf("hits = %+v", r.Results)
	}
	// from elsewhere the whole of it is searchable
	if r := f.search(Query{Text: "recall"}); len(r.Results) != 1 || r.Results[0].MatchMessageID != 2 {
		t.Errorf("from another session: %+v", r.Results)
	}
}

// Rows the runtime wrote, not people: compaction summaries, follow-up inputs, silent replies and
// tool output are never hits; carried rows are found once, in the session they came from.
func TestSearchSkipsMarkers(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Create(wire.Session{ID: "c1", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Append("c1",
		wire.Message{Role: "user", Content: "[Follow-up fu1 that you scheduled on Monday. No one wrote this now: do what it says]\nremind about pepino"},
		wire.Message{Role: "user", Content: "please check the berenjena report", Kind: ""},
		wire.Message{Role: "assistant", Content: "[SILENT]"},
		wire.Message{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "c1", Name: "terminal", Arguments: `{"command":"cat calabaza.txt"}`}}},
		wire.Message{Role: "tool", ToolCallID: "c1", Content: "calabaza contents"},
		wire.Message{Role: "assistant", Content: "The berenjena report is ready."}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Append("c1", wire.Message{Role: "user", Content: "[Follow-up fu2 that you scheduled]\npepino again", Kind: wire.KindFollowUp}); err != nil {
		t.Fatal(err)
	}
	_ = f.store.Sync("c1")
	for _, q := range []string{"calabaza", "silent", "scheduled", "summary"} {
		if r := f.search(Query{Text: q}); len(r.Results) != 0 {
			t.Errorf("%q found %v", q, ids(r))
		}
	}
	if r := f.search(Query{Text: "berenjena"}); len(r.Results) != 1 {
		t.Errorf("berenjena = %v", ids(r))
	}
	// a follow-up's input row (kind follow_up) is not what a person said
	r := f.search(Query{Text: "pepino"})
	for _, h := range r.Results {
		if strings.Contains(h.Snippet, "pepino again") {
			t.Errorf("a follow-up's input was a hit: %+v", h)
		}
	}
	tip, err := f.store.Compact("c1", "Summary: berenjena and calabaza.", []wire.Message{{Role: "assistant", Content: "The berenjena report is ready.", At: "2026-01-01T00:00:00.000Z"}})
	if err != nil {
		t.Fatal(err)
	}
	f.add(tip, "user", "thanks", "assistant", "You are welcome.")
	r = f.search(Query{Text: "berenjena", Limit: 10})
	if got := ids(r); len(got) != 1 || got[0] != "c1" {
		t.Errorf("carried rows or the summary found: %v", got)
	}
	// the window shows no summary row and names the tool a result came from
	m := f.scroll(Scroll{SessionID: "c1", Around: at(4), Window: 2})
	for _, x := range m.Messages {
		if x.Role == "system" {
			t.Errorf("summary shown: %+v", x)
		}
		if x.Role == "tool" && x.ToolName != "terminal" {
			t.Errorf("tool row = %+v", x)
		}
	}
}

func at(n int) *int { return &n }

func (f *fixture) scroll(s Scroll) ScrollResult {
	f.t.Helper()
	if s.Visible == nil {
		s.Visible = all
	}
	r, err := f.ix.Scroll(s)
	if err != nil {
		f.t.Fatalf("scroll %+v: %v", s, err)
	}
	return r
}

// The index is lazy and incremental: rows appended after a search are found by the next one, a
// session's end (its header rewritten) keeps what was indexed, a deleted session goes.
func TestIndexIncremental(t *testing.T) {
	f := newFixture(t)
	f.conv("c1", "api_server", "user", "first alcachofa", "assistant", "ok")
	if r := f.search(Query{Text: "alcachofa"}); len(r.Results) != 1 {
		t.Fatalf("first = %v", ids(r))
	}
	f.add("c1", "user", "then rabano", "assistant", "fine")
	if err := f.store.End("c1", "oneshot"); err != nil {
		t.Fatal(err)
	}
	r := f.search(Query{Text: "rabano"})
	if len(r.Results) != 1 || r.Results[0].MatchMessageID != 2 {
		t.Fatalf("appended = %+v", r.Results)
	}
	if r := f.search(Query{Text: "alcachofa"}); len(r.Results) != 1 || r.Results[0].MatchMessageID != 0 {
		t.Errorf("after the end = %+v", r.Results)
	}
	// a torn last line (a write in progress) is not read until it is whole
	fh, _ := os.OpenFile(filepath.Join(f.dir, "c1.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	_, _ = fh.WriteString(`{"role":"user","content":"half puer`)
	_ = fh.Close()
	if r := f.search(Query{Text: "puerro"}); len(r.Results) != 0 {
		t.Errorf("torn line indexed: %v", ids(r))
	}
	fh, _ = os.OpenFile(filepath.Join(f.dir, "c1.jsonl"), os.O_WRONLY|os.O_APPEND, 0)
	_, _ = fh.WriteString(`ro now","at":"2026-10-20T08:00:00.000Z"}` + "\n")
	_ = fh.Close()
	if r := f.search(Query{Text: "puerro"}); len(r.Results) != 1 || r.Results[0].MatchMessageID != 4 {
		t.Errorf("completed line = %+v", r.Results)
	}
	if _, err := f.store.DeleteLineage("c1"); err != nil {
		t.Fatal(err)
	}
	if r := f.search(Query{Text: "alcachofa"}); len(r.Results) != 0 {
		t.Errorf("deleted session found: %v", ids(r))
	}
	// a session made again under the same id is read afresh
	f.conv("c1", "api_server", "user", "nabo only")
	if r := f.search(Query{Text: "alcachofa"}); len(r.Results) != 0 {
		t.Errorf("old content survived: %v", ids(r))
	}
	if r := f.search(Query{Text: "nabo"}); len(r.Results) != 1 {
		t.Errorf("new content = %v", ids(r))
	}
}

// Filters: the session's start (after, before; dates and relative durations), its source, the
// roles, ids already seen (with their lineage), and newest or oldest first.
func TestSearchFilters(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	for i, c := range []struct{ id, source, created string }{
		{"old", "api_server", now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)},
		{"mid", "cron", now.Add(-3 * 24 * time.Hour).Format(time.RFC3339)},
		{"new", "api_server", now.Add(-1 * time.Hour).Format(time.RFC3339)},
	} {
		if _, err := f.store.Create(wire.Session{ID: c.id, Source: c.source, Created: c.created}); err != nil {
			t.Fatal(err)
		}
		f.add(c.id, "user", fmt.Sprintf("tomate question %d", i), "assistant", "lechuga answer")
	}
	cases := []struct {
		q    Query
		want string
	}{
		{Query{Text: "tomate", After: "7d", Limit: 10}, "new,mid"},
		{Query{Text: "tomate", Before: "7d", Limit: 10}, "old"},
		{Query{Text: "tomate", After: now.Add(-48 * time.Hour).Format("2006-01-02"), Limit: 10}, "new"},
		{Query{Text: "tomate", Source: "cron", Limit: 10}, "mid"},
		{Query{Text: "lechuga", Roles: "user", Limit: 10}, ""},
		{Query{Text: "lechuga", Roles: "assistant", Sort: "oldest", Limit: 10}, "old,mid,new"},
		{Query{Text: "tomate", Sort: "newest", Limit: 10}, "new,mid,old"},
		{Query{Text: "tomate", Exclude: []string{"new", "old"}, Limit: 10}, "mid"},
		{Query{Text: "tomate", Sort: "newest", Limit: 2}, "new,mid"},
	}
	for _, c := range cases {
		if got := strings.Join(ids(f.search(c.q)), ","); got != c.want {
			t.Errorf("%+v: %q, want %q", c.q, got, c.want)
		}
	}
	for _, bad := range []Query{{Text: "x", After: "yesterday"}, {Text: "x", Roles: "tool"}, {Text: "x", Sort: "random"}} {
		bad.Visible = all
		if _, err := f.ix.Search(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	// cron sessions rank below the others with the same words (Hermes' demotion)
	if got := ids(f.search(Query{Text: "tomate", Limit: 10})); got[len(got)-1] != "mid" {
		t.Errorf("cron not demoted: %v", got)
	}
}

// Scroll is a window of a session's messages around one, with how many lie before and after;
// read is a session's head and tail; browse is the recent conversations the caller may see.
func TestScrollReadBrowse(t *testing.T) {
	f := newFixture(t)
	rows := []string{}
	for i := range 30 {
		rows = append(rows, "user", fmt.Sprintf("question %d", i), "assistant", fmt.Sprintf("answer %d %s", i, strings.Repeat("long ", 400)))
	}
	f.conv("c1", "api_server", rows...)
	f.conv("c2", "slack", "user", "hidden")
	m := f.scroll(Scroll{SessionID: "c1", Around: at(10), Window: 2})
	if len(m.Messages) != 5 || m.Messages[0].ID != 8 || m.Messages[4].ID != 12 || !m.Messages[2].Anchor || m.MessagesBefore != 8 || m.MessagesAfter != 47 {
		t.Errorf("scroll = %+v", m)
	}
	for _, x := range m.Messages {
		if len(x.Content) > 1100 || x.Role == "assistant" && !x.Cut {
			t.Errorf("message not bounded: %d %v", len(x.Content), x.Cut)
		}
	}
	m = f.scroll(Scroll{SessionID: "c1", Around: at(0), Window: 50})
	if len(m.Messages) != 21 || m.Window != 20 {
		t.Errorf("window clamp = %d, %d", len(m.Messages), m.Window)
	}
	m = f.scroll(Scroll{SessionID: "c1"})
	if m.MessageCount != 60 || len(m.Messages) != 20 || m.Messages[0].ID != 0 || m.Messages[19].ID != 59 || !m.Truncated {
		t.Errorf("read = %d messages, count %d", len(m.Messages), m.MessageCount)
	}
	for _, s := range []Scroll{{SessionID: "c2", Visible: func(root, _ string) bool { return root != "c2" }}, {SessionID: "c1", Current: "c1"}, {SessionID: "nope"}, {SessionID: "c1", Around: at(99)}} {
		if s.Visible == nil {
			s.Visible = all
		}
		if _, err := f.ix.Scroll(s); err == nil {
			t.Errorf("%+v answered", s)
		}
	}
	b, err := f.ix.Browse(Browse{Limit: 5, Current: "c1", Visible: func(root, _ string) bool { return true }})
	if err != nil || len(b.Results) != 1 || b.Results[0].SessionID != "c2" || b.Results[0].Preview != "hidden" {
		t.Errorf("browse = %+v %v", b, err)
	}
}

// Text the runtime shows from a past conversation has credential-shaped strings masked (a known
// secret value is masked by the tool's redactor), and snippets are bounded.
func TestSearchMasksAndBounds(t *testing.T) {
	f := newFixture(t)
	f.conv("c1", "api_server", "user", "the coliflor key is sk_live_abcdefghijklmnop "+strings.Repeat("filler ", 300)+" end")
	r := f.search(Query{Text: "coliflor"})
	if len(r.Results) != 1 {
		t.Fatalf("found %v", ids(r))
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "sk_live") || !strings.Contains(string(b), "[SECRET]") {
		t.Errorf("not masked: %s", b)
	}
	if n := len([]rune(r.Results[0].Snippet)); n > 320 {
		t.Errorf("snippet %d runes", n)
	}
}

// The index stays within its bound: past MaxPostings the oldest sessions leave it, and a search
// says from when it covers.
func TestIndexBound(t *testing.T) {
	f := newFixture(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 40 {
		id := fmt.Sprintf("s%02d", i)
		if _, err := f.store.Create(wire.Session{ID: id, Source: "api_server", Created: base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339)}); err != nil {
			t.Fatal(err)
		}
		words := []string{"shared"}
		for j := range 20 {
			words = append(words, fmt.Sprintf("w%dx%d", i, j))
		}
		f.add(id, "user", strings.Join(words, " "))
	}
	f.ix.MaxPostings = 200
	r := f.search(Query{Text: "shared", Limit: 10, Sort: "oldest"})
	if r.IndexedSince == "" || len(r.Results) == 0 || r.Results[0].SessionID == "s00" {
		t.Errorf("bounded: since %q, %v", r.IndexedSince, ids(r))
	}
	if n := f.ix.postings(); n > 200 {
		t.Errorf("%d postings over the bound", n)
	}
	if r := f.search(Query{Text: "w39x5"}); len(r.Results) != 1 {
		t.Errorf("newest session not indexed: %v", ids(r))
	}
}

// A conversation's root (whose audience decides who sees it) is known from every file's header,
// even when the bound left the root session itself out of the index.
func TestRootSurvivesTheBound(t *testing.T) {
	f := newFixture(t)
	words := []string{}
	for j := range 60 {
		words = append(words, fmt.Sprintf("old%d", j))
	}
	f.conv("slack-D1-dm", "slack", "user", strings.Join(words, " "))
	time.Sleep(5 * time.Millisecond)
	tip, err := f.store.Compact("slack-D1-dm", "summary", nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	f.add(tip, "user", "the chirivia plan", "assistant", "ok")
	f.ix.MaxPostings = 10
	var roots []string
	r := f.search(Query{Text: "chirivia", Visible: func(root, source string) bool { roots = append(roots, root+"|"+source); return true }})
	if len(r.Results) != 1 || r.IndexedSince == "" {
		t.Fatalf("search = %+v", r)
	}
	if len(roots) != 1 || roots[0] != "slack-D1-dm|slack" {
		t.Errorf("visibility asked for %v", roots)
	}
	if r.Results[0].Conversation != "slack-D1-dm" {
		t.Errorf("hit = %+v", r.Results[0])
	}
	// a session left out of the index can still be read
	if m := f.scroll(Scroll{SessionID: "slack-D1-dm"}); m.MessageCount != 1 {
		t.Errorf("read of an unindexed session = %+v", m)
	}
}
