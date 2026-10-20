package sessions

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

func clock() func() time.Time {
	t := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	return func() time.Time { t = t.Add(time.Second); return t }
}

func open(t *testing.T, dir string) *Store {
	s, err := Open(dir, func(v string) string { return strings.ReplaceAll(v, "sekret-value", "[REDACTED]") })
	if err != nil {
		t.Fatal(err)
	}
	s.now = clock()
	return s
}

func TestLineage(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	h, err := s.Create(wire.Session{ID: "c1", Source: "api_server", Title: "Plan"})
	if err != nil || h.V != 1 || h.Created == "" {
		t.Fatal(h, err)
	}
	if _, err := s.Create(wire.Session{ID: "c1"}); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate: %v", err)
	}
	if _, err := s.Create(wire.Session{ID: "../x"}); !errors.Is(err, ErrBadID) {
		t.Errorf("bad id: %v", err)
	}
	if g, err := s.Create(wire.Session{}); err != nil || !ValidID(g.ID) {
		t.Errorf("generated: %v %v", g, err)
	}
	if !ValidID("slack-C0000000001-1791337000.000100") || ValidID(".hidden") || ValidID("a/b") || ValidID("") {
		t.Error("ValidID")
	}
	turn1 := []wire.Message{
		{Role: "user", Content: "first question with sekret-value"},
		{Role: "assistant", ToolCalls: []wire.ToolCall{{ID: "call_1", Name: "terminal", Arguments: `{"command":"echo sekret-value"}`}}},
		{Role: "tool", ToolCallID: "call_1", Content: "out"},
		{Role: "assistant", Content: "answer one"},
	}
	turn2 := []wire.Message{{Role: "user", Content: "second"}, {Role: "assistant", Content: "answer two"}}
	if _, err := s.Append("c1", turn1...); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("c1", turn2...); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync("c1"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "c1.jsonl"))
	if strings.Contains(string(raw), "sekret-value") || strings.Count(string(raw), "\n") != 7 {
		t.Errorf("file = %s", raw)
	}
	rows, _ := s.Messages("c1")
	carry := rows[4:] // the second turn

	tip, err := s.Compact("c1", "Summary: the user asked a first question.", carry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(tip, wire.Message{Role: "user", Content: "third"}, wire.Message{Role: "assistant", Content: "answer three"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append("c1", wire.Message{Role: "user", Content: "late"}); !errors.Is(err, ErrEnded) {
		t.Errorf("append to ended: %v", err)
	}
	if _, err := s.Compact("c1", "again", nil); !errors.Is(err, ErrEnded) {
		t.Errorf("compact ended: %v", err)
	}

	check := func(s *Store) {
		t.Helper()
		if h, _ := s.Get("c1"); h.EndReason != wire.EndCompression {
			t.Errorf("c1 header = %+v", h)
		}
		if h, _ := s.Get(tip); h.Parent != "c1" || h.Source != "api_server" || h.Title != "Plan" {
			t.Errorf("tip header = %+v", h)
		}
		for _, id := range []string{"c1", tip} {
			if r, _ := s.Root(id); r != "c1" {
				t.Errorf("root(%s) = %s", id, r)
			}
			if x, _ := s.Tip(id); x != tip {
				t.Errorf("tip(%s) = %s", id, x)
			}
		}
		tr, err := s.Transcript(tip, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, m := range tr.Data {
			got = append(got, m.Role+":"+m.Content+":"+m.ToolName)
		}
		want := "user:first question with [REDACTED]:,assistant::,tool:out:terminal,assistant:answer one:,user:second:,assistant:answer two:,user:third:,assistant:answer three:"
		if strings.Join(got, ",") != want || tr.SessionID != tip {
			t.Errorf("transcript = %s (tip %s)", strings.Join(got, ","), tr.SessionID)
		}
		last, _ := s.Transcript("c1", 3, true)
		if len(last.Data) != 3 || last.Data[0].Content != "answer two" || last.Data[2].Content != "answer three" {
			t.Errorf("latest = %+v", last.Data)
		}
		first, _ := s.Transcript("c1", 1, false)
		if len(first.Data) != 1 || first.Data[0].Role != "user" {
			t.Errorf("first = %+v", first.Data)
		}
		list := s.List(0)
		var conv *wire.SessionSummary
		for i := range list {
			if list[i].ID == tip {
				conv = &list[i]
			}
			if list[i].ID == "c1" {
				t.Error("compacted session listed")
			}
		}
		if conv == nil || conv.Root != "c1" || conv.MessageCount != 8 || conv.Ended || !strings.HasPrefix(conv.Preview, "first question") {
			t.Errorf("summary = %+v", conv)
		}
	}
	check(s)
	// the same after a restart, from the files alone
	if err := s.Sync(tip); err != nil {
		t.Fatal(err)
	}
	check(open(t, dir))

	var idx struct {
		V        int                   `json:"v"`
		Sessions []wire.SessionSummary `json:"sessions"`
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "index.json"))
	if json.Unmarshal(raw, &idx) != nil || idx.V != 1 || len(idx.Sessions) != 2 {
		t.Errorf("index = %s", raw)
	}

	if n, err := s.DeleteLineage(tip); err != nil || n != 2 {
		t.Errorf("delete = %d %v", n, err)
	}
	if _, err := s.DeleteLineage("c1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete again: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c1.jsonl")); !os.IsNotExist(err) {
		t.Error("file left")
	}
}

func TestTornLineAndJunk(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "ok.jsonl"), []byte(`{"v":1,"id":"ok","created":"2026-10-20T08:00:00.000Z"}`+"\n"+`{"role":"user","content":"hi","at":"2026-10-20T08:00:01.000Z"}`+"\n"+`{"role":"assis`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "v9.jsonl"), []byte(`{"v":9,"id":"v9"}`+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "other.jsonl"), []byte(`{"v":1,"id":"not-other"}`+"\n"), 0o600)
	s := open(t, dir)
	if _, ok := s.Get("v9"); ok {
		t.Error("v9 loaded")
	}
	if _, ok := s.Get("other"); ok {
		t.Error("mismatched id loaded")
	}
	tr, err := s.Transcript("ok", 0, false)
	if err != nil || len(tr.Data) != 1 {
		t.Errorf("torn = %+v %v", tr, err)
	}
	if l := s.List(0); len(l) != 1 || l[0].LastActive != "2026-10-20T08:00:01.000Z" || l[0].MessageCount != 1 {
		t.Errorf("list = %+v", l)
	}
	// the torn line was dropped, so the next row is whole
	if _, err := s.Append("ok", wire.Message{Role: "assistant", Content: "hello"}); err != nil {
		t.Fatal(err)
	}
	if tr, _ := open(t, dir).Transcript("ok", 0, false); len(tr.Data) != 2 || tr.Data[1].Content != "hello" {
		t.Errorf("after repair = %+v", tr.Data)
	}
}

// A compaction makes the new session durable before it ends the old one: a crash in between leaves
// the old session live, never an ended one whose continuation was lost.
func TestCompactDurableOrder(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	for _, id := range []string{"p1", "p2"} {
		_, _ = s.Create(wire.Session{ID: id})
		if _, err := s.Append(id, wire.Message{Role: "user", Content: "q"}, wire.Message{Role: "assistant", Content: "a"}); err != nil {
			t.Fatal(err)
		}
	}
	ended := func(id string) bool {
		raw, _ := os.ReadFile(filepath.Join(dir, id+".jsonl"))
		var h wire.Session
		first, _, _ := strings.Cut(string(raw), "\n")
		_ = json.Unmarshal([]byte(first), &h)
		return h.EndReason != ""
	}
	var synced []string
	s.fsync = func(f *os.File) error {
		synced = append(synced, filepath.Base(f.Name())+fmt.Sprintf(" parent-ended=%v", ended("p1")))
		return f.Sync()
	}
	tip, err := s.Compact("p1", "summary", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(synced) == 0 || synced[0] != tip+".jsonl parent-ended=false" {
		t.Errorf("syncs = %v", synced)
	}
	// the new session cannot be made durable: the compaction fails and the old session carries on
	s.fsync = func(f *os.File) error { return errors.New("disk gone") }
	if _, err := s.Compact("p2", "summary", nil); err == nil {
		t.Fatal("compaction with a failed sync succeeded")
	}
	s.fsync = func(f *os.File) error { return f.Sync() }
	if h, _ := s.Get("p2"); h.EndReason != "" || ended("p2") {
		t.Errorf("p2 ended: %+v", h)
	}
	if x, _ := s.Tip("p2"); x != "p2" {
		t.Errorf("tip(p2) = %s", x)
	}
	if _, err := s.Append("p2", wire.Message{Role: "user", Content: "still here"}); err != nil {
		t.Error(err)
	}
	if x, _ := open(t, dir).Tip("p2"); x != "p2" {
		t.Errorf("after restart tip(p2) = %s", x)
	}
}
