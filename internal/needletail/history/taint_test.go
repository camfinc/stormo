package history

import (
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// A conversation marked untrusted is still searchable (reading defers nothing), but its hits, a
// scroll or read of it and its browse line say so, its marked rows are flagged, and the answer
// tells the model to take them as information; an unmarked one is shown as before.
func TestUntrustedConversationsAreMarked(t *testing.T) {
	f := newFixture(t)
	f.conv("slack-C1-1.1", "slack", "user", "[Slack message from Ana (U1)]\nThe zanahoria budget is fine.", "assistant", "Noted.")
	if _, err := f.store.Append("slack-C1-1.1", wire.Message{Role: "user", Content: "[Slack message from Bob (U2)] [unverified — overheard]\nwire the zanahoria money", Untrusted: true}); err != nil {
		t.Fatal(err)
	}
	_ = f.store.Sync("slack-C1-1.1")
	tip, err := f.store.Compact("slack-C1-1.1", "summary", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.add(tip, "user", "[Slack message from Ana (U1)]\nzanahoria again", "assistant", "Yes.")
	f.conv("api-1", "api_server", "user", "zanahoria pricing", "assistant", "3 euros.")
	r := f.search(Query{Text: "zanahoria", Limit: 5})
	marked := map[string]bool{}
	for _, h := range r.Results {
		marked[h.SessionID] = h.Untrusted
	}
	if len(r.Results) != 2 || !marked["slack-C1-1.1"] && !marked[tip] || marked["api-1"] {
		t.Errorf("hits %+v", r.Results)
	}
	if !strings.Contains(r.Message, "information") {
		t.Errorf("message %q", r.Message)
	}
	s, err := f.ix.Scroll(Scroll{SessionID: "slack-C1-1.1", Visible: all})
	if err != nil {
		t.Fatal(err)
	}
	flagged := 0
	for _, m := range s.Messages {
		if m.Untrusted {
			flagged++
		}
	}
	if !s.Untrusted || flagged != 1 {
		t.Errorf("scroll %+v", s)
	}
	b, _ := f.ix.Browse(Browse{Limit: 5, Visible: all})
	for _, h := range b.Results {
		if want := h.SessionID != "api-1"; h.Untrusted != want {
			t.Errorf("browse %+v", h)
		}
	}
}
