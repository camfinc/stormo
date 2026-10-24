package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/api"
	"github.com/camfinc/stormo/internal/needletail/followup"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// A follow-up text made from input the agent may not obey (marked untrusted) or by its handler
// (luau:…) keeps that provenance when delivered: the row kept in its conversation is marked, and
// so the conversation is; the model's own text from a trusted run is kept as before.
func TestDeliveredFollowUpKeepsItsProvenance(t *testing.T) {
	dir := t.TempDir()
	store, err := sessions.Open(filepath.Join(dir, "sessions"), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "state"), 0o700)
	srv, err := api.New(api.Options{Store: store, StateDir: filepath.Join(dir, "state"), TmpDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	f := &followUps{store: store, srv: srv, logf: func(string) {}, loc: time.UTC}
	for _, id := range []string{"chat-a", "chat-b", "chat-c"} {
		if _, err := store.Create(wire.Session{ID: id, Source: "api_server"}); err != nil {
			t.Fatal(err)
		}
	}
	items := map[string]followup.Item{
		"chat-a": {ID: "fu1", Kind: followup.KindMessage, Text: "I'll look at it.", Untrusted: true, By: "model"},
		"chat-b": {ID: "fu2", Kind: followup.KindMessage, Text: "Hours: 9 to 5.", By: "luau:ops__faq"},
		"chat-c": {ID: "fu3", Kind: followup.KindMessage, Text: "Hola.", By: "model"},
	}
	for id, it := range items {
		it.Target, it.Origin = followup.TargetOrigin, followup.Conversation{Session: id, Source: "api_server"}
		if err := f.Fire(context.Background(), it); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
	}
	for id, want := range map[string]bool{"chat-a": true, "chat-b": true, "chat-c": false} {
		rows, _ := store.Messages(id)
		if len(rows) != 1 || rows[0].Untrusted != want || store.Untrusted(id) != want {
			t.Errorf("%s: rows %+v, conversation marked %v, want %v", id, rows, store.Untrusted(id), want)
		}
	}
	// the conversation's mark reaches what the follow-up tools see of it
	origin := followUpOrigin(store, nil)
	if !origin("chat-a").Untrusted || origin("chat-c").Untrusted {
		t.Errorf("origins: %+v %+v", origin("chat-a"), origin("chat-c"))
	}
	if why := untrustedRun(store)(context.Background(), "chat-b"); !strings.Contains(why, "may not obey") {
		t.Errorf("the tools' check: %q", why)
	}
	if why := untrustedRun(store)(context.Background(), "chat-c"); why != "" {
		t.Errorf("an unmarked conversation: %q", why)
	}
}
