package loop

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Exercise real recall at the request boundary, including retries and compaction. The
// deterministic model stub tests request correctness, not stochastic answer quality.
func TestRecallReplayRequestsAndTranscript(t *testing.T) {
	st := &stub{pick: func(map[string]any) string { return say("Noted.") }}
	srv := httptest.NewServer(st)
	defer srv.Close()
	a := newAgent(t, srv)
	if _, err := a.Store.Create(wire.Session{ID: "recall-replay", Source: "api_server"}); err != nil {
		t.Fatal(err)
	}
	ms := &memory.Store{Home: a.Cfg.Home}
	saved, err := ms.Save(memory.SaveOptions{Text: "Acme ferry deposits require twenty percent payment."})
	if err != nil {
		t.Fatal(err)
	}
	rc := &memory.Recaller{Store: ms, Auto: true, Root: a.Store.Root}
	a.Recall = rc.Block
	session := "recall-replay"
	query := "What payment do Acme ferry deposits require?"
	run := func(want string) {
		t.Helper()
		st.requests = nil
		if _, err := a.Run(context.Background(), session, query, Options{}); err != nil {
			t.Fatal(err)
		}
		if len(st.requests) != 1 {
			t.Fatalf("requests: %d", len(st.requests))
		}
		req := st.requests[0]
		if got := fmt.Sprint(lastRow(req)["content"]); !strings.Contains(got, want) {
			t.Fatalf("request missing %q: %s", want, got)
		}
		if strings.Contains(fmt.Sprint(req["messages"].([]any)[0]), "memory-context") {
			t.Fatal("recall in system prompt")
		}
	}
	run("twenty")
	run("twenty")
	revised := "Acme ferry deposits require thirty percent payment."
	if _, err := ms.Update(memory.Audience{}, saved.ID, memory.Change{Text: &revised}, false); err != nil {
		t.Fatal(err)
	}
	run("thirty")
	child, err := a.Store.Compact(session, "The user asked about ferry deposits.", nil)
	if err != nil {
		t.Fatal(err)
	}
	session = child
	run("thirty")
	// A cancelled attempt cannot consume the next turn's eligibility.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = a.Run(ctx, session, query, Options{})
	run("thirty")
	if err := ms.Delete(memory.Audience{}, saved.ID, false); err != nil {
		t.Fatal(err)
	}
	run("")
	if strings.Contains(fmt.Sprint(lastRow(st.requests[0])["content"]), "memory-context") {
		t.Fatal("deleted record injected")
	}
	files, err := filepath.Glob(filepath.Join(a.Cfg.Home, "sessions", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "memory-context") || strings.Contains(string(raw), "percent payment") {
			t.Fatalf("recalled text persisted in %s", file)
		}
	}
}
