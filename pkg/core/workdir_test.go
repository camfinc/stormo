package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/engine/hermes"
)

type wdFixture struct {
	w      *Workdir
	m      *Monitor
	db     *DB
	root   string
	now    time.Time
	roster []BusAgent
}

func newWorkdir(t *testing.T) *wdFixture {
	t.Helper()
	f := &wdFixture{db: testDB(t), root: filepath.Join(t.TempDir(), "workdir"), now: time.Now()}
	f.roster = []BusAgent{{ID: "atlas", Unit: "sales", Running: true}, {ID: "nova", Unit: "media", Running: true}, {ID: "scout", Unit: "sales", Running: true}}
	f.m = NewMonitor(f.db, testKeys(t), nil)
	f.w = NewWorkdir(WorkdirOptions{Root: f.root, DB: f.db, Monitor: f.m, Roster: func() []BusAgent { return f.roster }, Now: func() time.Time { return f.now }})
	for _, l := range []string{"group", "sales", "media"} {
		if err := os.MkdirAll(filepath.Join(f.root, l), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *wdFixture) file(t *testing.T, key, body string) {
	t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(key))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantToolErr(t *testing.T, err error, contains string) {
	t.Helper()
	if _, ok := err.(ToolError); !ok || !strings.Contains(err.Error(), contains) {
		t.Errorf("got %v, want a tool error containing %q", err, contains)
	}
}

// Phase 4's done-when, first half: two agents contend for a file; the second sees the lock and holder.
func TestLocksContention(t *testing.T) {
	f := newWorkdir(t)
	r, err := f.w.Lock("atlas", "/shared/group/plans/q4.md", "hard", "drafting the Q4 plan", 0)
	if err != nil || r.Lock.Owner != "atlas" || r.Lock.Path != "/shared/group/plans/q4.md" || len(r.Warnings) != 0 {
		t.Fatalf("lock %+v %v", r, err)
	}
	_, err = f.w.Lock("nova", "/shared/group/plans/q4.md", "hard", "", 0)
	wantToolErr(t, err, "atlas holds a hard lock on /shared/group/plans/q4.md")
	if err != nil && !strings.Contains(err.Error(), "drafting the Q4 plan") {
		t.Errorf("the reason is shown: %v", err)
	}
	// A lock on the directory or a glob over it conflicts too.
	_, err = f.w.Lock("nova", "/shared/group/plans", "temp", "", 0)
	wantToolErr(t, err, "atlas holds")
	_, err = f.w.Lock("nova", "/shared/group/**/*.md", "hard", "", 0)
	wantToolErr(t, err, "atlas holds")
	// A soft lock beside it is allowed, with the holder named.
	soft, err := f.w.Lock("nova", "/shared/group/plans/q4.md", "soft", "reviewing", 0)
	if err != nil || len(soft.Warnings) != 1 || !strings.Contains(soft.Warnings[0], "atlas") {
		t.Fatalf("soft %+v %v", soft, err)
	}
	info, err := f.w.Info("nova", "/shared/group/plans/q4.md")
	if err != nil || len(info.Locks) != 2 || len(info.Warnings) != 1 || !strings.Contains(info.Warnings[0], "atlas holds a hard lock") {
		t.Fatalf("info %+v %v", info, err)
	}
	_, err = f.w.Write("nova", "/shared/group/plans/q4.md", "nova's version", "")
	wantToolErr(t, err, "not written: atlas holds")
	// The holder writes through the core: exact attribution, and nova's soft lock only warns.
	wr, err := f.w.Write("atlas", "/shared/group/plans/q4.md", "# Q4\n", "create")
	if err != nil || !wr.Created || len(wr.Warnings) != 1 {
		t.Fatalf("write %+v %v", wr, err)
	}
	if _, err := f.w.Write("atlas", "/shared/group/plans/q4.md", "again", "create"); err == nil {
		t.Error("create over an existing file")
	}
	if _, err := f.w.Write("atlas", "/shared/group/plans/q4.md", "more\n", "append"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(f.root, "group/plans/q4.md"))
	if string(b) != "# Q4\nmore\n" {
		t.Errorf("content %q", b)
	}
	info, _ = f.w.Info("scout", "/shared/group/plans/q4.md")
	if info.Creator != "atlas" || info.LastWriter != "atlas" || info.How != "fs_write" || len(info.History) != 2 || info.History[0].Kind != "modified" {
		t.Errorf("attribution %+v", info)
	}
	// Released: nova may lock it now.
	if err := f.w.Unlock("atlas", "/shared/group/plans/q4.md"); err != nil {
		t.Fatal(err)
	}
	wantToolErr(t, f.w.Unlock("atlas", "/shared/group/plans/q4.md"), "no lock")
	if _, err := f.w.Lock("nova", "/shared/group/plans/q4.md", "hard", "", 0); err != nil {
		t.Errorf("after release: %v", err)
	}
	ls, _ := f.w.Locks("scout", "")
	if len(ls) != 1 || ls[0].Owner != "nova" || ls[0].Kind != "hard" {
		t.Errorf("locks %+v (nova's soft lock became hard: one lock per path and owner)", ls)
	}
	es, err := f.w.Ls("scout", "/shared/group/plans")
	if err != nil || len(es) != 1 || es[0].LastWriter != "atlas" || es[0].Locked != "nova (hard)" {
		t.Errorf("ls %+v %v", es, err)
	}
}

func TestLockKindsAndBoundaries(t *testing.T) {
	f := newWorkdir(t)
	tmp, err := f.w.Lock("atlas", "/shared/sales/pipeline.csv", "temp", "", 120)
	if err != nil {
		t.Fatal(err)
	}
	if exp, _ := time.Parse(time.RFC3339, tmp.Lock.Expires); exp.Sub(f.now) > lockTTL["temp"]+time.Second {
		t.Errorf("a temp lock ignores minutes: expires %s", tmp.Lock.Expires)
	}
	_, err = f.w.Renew("atlas", "/shared/sales/pipeline.csv", 0)
	wantToolErr(t, err, "never renewed")
	h, _ := f.w.Lock("atlas", "/shared/sales/report.md", "hard", "", 600)
	if exp, _ := time.Parse(time.RFC3339, h.Lock.Expires); exp.Sub(f.now) > maxLockTTL+time.Second {
		t.Errorf("lifetime capped at %s: %s", maxLockTTL, h.Lock.Expires)
	}
	f.now = f.now.Add(20 * time.Minute)
	if l, err := f.w.Renew("atlas", "/shared/sales/report.md", 0); err != nil || l.Expires != isoMs(f.now.Add(lockTTL["hard"]).UnixMilli()) {
		t.Errorf("renew %+v %v", l, err)
	}
	_, err = f.w.Lock("atlas", "/shared/sales/x", "exclusive", "", 0)
	wantToolErr(t, err, "kind is")
	for _, c := range []struct{ agent, path, want string }{
		{"atlas", "/shared/media/brief.md", "not mounted for you"},
		{"atlas", "/shared/sales/../media/brief.md", "not mounted for you"},
		{"atlas", "/opt/data/memories/MEMORY.md", "not in the shared space"},
		{"atlas", "notes.md", "absolute path"},
		{"atlas", "/shared/sales", "whole layer"},
		{"ghost", "/shared/group/x", "does not know"},
	} {
		_, err := f.w.Lock(c.agent, c.path, "hard", "", 0)
		wantToolErr(t, err, c.want)
	}
	_, err = f.w.Write("nova", "/shared/sales/pipeline.csv", "x", "")
	wantToolErr(t, err, "not mounted")
	// Agents only see locks in layers they mount.
	if ls, _ := f.w.Locks("nova", ""); len(ls) != 0 {
		t.Errorf("nova sees sales locks: %+v", ls)
	}
	// The temp lock expired after 5 minutes (20 have passed); the hard one is live.
	if ls, _ := f.w.Locks("scout", "/shared/sales"); len(ls) != 1 || ls[0].Kind != "hard" {
		t.Errorf("scout sees the live sales lock: %+v", ls)
	}
	if err := f.w.Sweep(); err != nil {
		t.Fatal(err)
	}
	var rows int
	_ = f.db.QueryRow(`SELECT COUNT(*) FROM locks`).Scan(&rows)
	if rows != 1 {
		t.Errorf("the sweep deletes expired locks: %d rows", rows)
	}
	f.roster[0].Running = false
	_ = f.w.Sweep()
	f.now = f.now.Add(lockGrace - time.Second)
	_ = f.w.Sweep()
	if ls, _ := f.w.Locks("scout", ""); len(ls) != 1 {
		t.Errorf("kept within the grace period: %+v", ls)
	}
	f.now = f.now.Add(2 * time.Second)
	_ = f.w.Sweep()
	if ls, _ := f.w.Locks("scout", ""); len(ls) != 0 {
		t.Errorf("a stopped owner's locks are released: %+v", ls)
	}
}

func ingest(t *testing.T, m *Monitor, agent, event, tool string, input map[string]any, call string, at time.Time) {
	t.Helper()
	deliverySeq++
	body, _ := json.Marshal(map[string]any{"hook_event_name": event, "tool_name": tool, "tool_input": input, "session_id": "s",
		"delivery_id": "x" + string(rune('a'+deliverySeq%26)) + time.Now().Format("150405.000000000"),
		"timestamp":   at.Format(time.RFC3339Nano), "extra": map[string]any{"tool_call_id": call}})
	e, err := hermes.NewRuntime().ParseHook(body) // a Hermes delivery, as the core reads it
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ingest(agent, e); err != nil {
		t.Fatal(err)
	}
}

// Phase 4's done-when, second half: a terminal write is attributed.
func TestScanAttribution(t *testing.T) {
	f := newWorkdir(t)
	f.file(t, "group/old.md", "before the core")
	if err := f.w.Scan(); err != nil {
		t.Fatal(err)
	}
	if h, _ := f.w.history("group/old.md", 5); len(h) != 0 {
		t.Errorf("the first scan only takes stock: %+v", h)
	}

	// nova runs a terminal command that writes a file.
	t0 := f.now
	ingest(t, f.m, "nova", "pre_tool_call", "terminal", map[string]any{"command": "make report"}, "c1", t0)
	f.file(t, "group/report.md", "made by a script")
	mtime := t0.Add(time.Second)
	_ = os.Chtimes(filepath.Join(f.root, "group/report.md"), mtime, mtime)
	ingest(t, f.m, "nova", "post_tool_call", "terminal", nil, "c1", t0.Add(3*time.Second))

	// atlas writes with its file tool (the hook names the path).
	f.file(t, "sales/notes.md", "tool write")
	ingest(t, f.m, "atlas", "pre_tool_call", "write_file", map[string]any{"path": "/shared/sales/notes.md"}, "c2", time.Now())
	ingest(t, f.m, "atlas", "post_tool_call", "write_file", map[string]any{"path": "/shared/sales/notes.md"}, "c2", time.Now())

	// Someone changes a file with no tool running: unknown, until scout notes it.
	f.file(t, "sales/mystery.md", "?")
	_ = os.Chtimes(filepath.Join(f.root, "sales/mystery.md"), t0.Add(-time.Hour), t0.Add(-time.Hour))

	f.now = time.Now().Add(5 * time.Second)
	if err := f.w.Scan(); err != nil {
		t.Fatal(err)
	}
	check := func(agent, path, writer, how string) {
		t.Helper()
		info, err := f.w.Info(agent, path)
		if err != nil {
			t.Fatal(err)
		}
		if info.LastWriter != writer || info.How != how || len(info.History) == 0 || info.History[0].Kind != "created" {
			t.Errorf("%s: writer %q how %q history %+v", path, info.LastWriter, info.How, info.History)
		}
	}
	check("atlas", "/shared/group/report.md", "nova", "terminal (inferred)")
	check("atlas", "/shared/sales/notes.md", "atlas", "tool:write_file")
	check("scout", "/shared/sales/mystery.md", "unknown", "scan")
	if err := f.w.Note("scout", "/shared/sales/mystery.md", "created"); err != nil {
		t.Fatal(err)
	}
	check("scout", "/shared/sales/mystery.md", "scout", "note")

	// Two agents at the terminal at once: nobody can be named.
	t1 := f.now
	ingest(t, f.m, "nova", "pre_tool_call", "terminal", nil, "c3", t1)
	ingest(t, f.m, "atlas", "pre_tool_call", "execute_code", nil, "c4", t1)
	f.file(t, "group/both.md", "x")
	_ = os.Chtimes(filepath.Join(f.root, "group/both.md"), t1.Add(time.Second), t1.Add(time.Second))
	_ = f.w.Scan()
	check("nova", "/shared/group/both.md", "unknown", "scan")
	// …unless only one of them can see the layer.
	f.file(t, "sales/only.md", "x")
	_ = os.Chtimes(filepath.Join(f.root, "sales/only.md"), t1.Add(time.Second), t1.Add(time.Second))
	_ = f.w.Scan()
	check("atlas", "/shared/sales/only.md", "atlas", "terminal (inferred)")

	// A touch is not a change; a deletion is.
	later := t1.Add(time.Minute)
	_ = os.Chtimes(filepath.Join(f.root, "group/old.md"), later, later)
	_ = os.Remove(filepath.Join(f.root, "group/report.md"))
	_ = f.w.Scan()
	if h, _ := f.w.history("group/old.md", 5); len(h) != 0 {
		t.Errorf("touch recorded: %+v", h)
	}
	if h, _ := f.w.history("group/report.md", 5); len(h) != 2 || h[0].Kind != "deleted" {
		t.Errorf("deletion: %+v", h)
	}
}

func TestWriteUnderAnotherAgentsLockIsAFinding(t *testing.T) {
	f := newWorkdir(t)
	f.file(t, "group/brief.md", "v1")
	_ = f.w.Scan()
	if _, err := f.w.Lock("atlas", "/shared/group/brief.md", "hard", "editing", 0); err != nil {
		t.Fatal(err)
	}
	t0 := f.now.Add(time.Second)
	ingest(t, f.m, "nova", "pre_tool_call", "terminal", nil, "c9", t0)
	f.file(t, "group/brief.md", "v2 by nova")
	_ = os.Chtimes(filepath.Join(f.root, "group/brief.md"), t0.Add(time.Second), t0.Add(time.Second))
	_ = f.w.Scan()
	h, _ := f.w.history("group/brief.md", 5)
	if len(h) != 1 || h[0].Agent != "nova" || h[0].Violation != "atlas" {
		t.Fatalf("history %+v", h)
	}
	if got := strings.Join(findingRules(t, f.db), ","); got != "nova:lock_violation" {
		t.Errorf("findings %s", got)
	}
	fs, _ := f.db.Findings(false, 5)
	if fs[0].Severity != "high" || !strings.Contains(fs[0].Evidence, "under atlas's hard lock") {
		t.Errorf("finding %+v", fs[0])
	}
	// The owner's view: locks, changes with paths, files per layer.
	v, err := f.w.Owner(10)
	if err != nil || len(v.Locks) != 1 || len(v.Changes) != 1 || v.Changes[0].Path != "/shared/group/brief.md" || v.Layers["group"] != 1 {
		t.Errorf("owner view %+v %v", v, err)
	}
	if c := f.w.LockCounts(); c["atlas"] != 1 || c["nova"] != 0 {
		t.Errorf("lock counts %v", c)
	}
}

func TestOverlapAndCovers(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"group/a.md", "group/a.md", true},
		{"group/dir", "group/dir/x.md", true},
		{"group/dir", "group/dirx/x.md", false},
		{"group/*.md", "group/a.md", true},
		{"group/*.md", "group/sub/a.md", false},
		{"group/**/*.md", "group/sub/a.md", true},
		{"group/rep*", "group/reports/q4.md", true},
		{"group/a/*.md", "group/a/**", true},
		{"group/a/*.md", "group/b/*.md", false},
		{"sales/x", "group/x", false},
	} {
		if got := overlap(c.a, c.b); got != c.want {
			t.Errorf("overlap(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
