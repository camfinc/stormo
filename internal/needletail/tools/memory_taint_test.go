package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// Item 13 (Codex 3): memory_manage's answers that show a marked record's text (a save's "similar"
// records, an update's echo) mark the reader first, durably; a mark that cannot be kept withholds
// the text.
func TestMemoryManageResultsCarryTheMark(t *testing.T) {
	r, env := setup(t)
	env.Memory.Limits = map[string]int{"memory": 2000, "user": 2000}
	res, err := env.Memory.Save(memory.SaveOptions{Text: "Ana prefers balcony cabins on cruises.", Kind: memory.KindPreference, Src: "chat", Scope: memory.ScopeAgent, Untrusted: true, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"action": "save", "text": "Ana prefers balcony cabins on cruises!", "scope": "agent"},
		{"action": "update", "id": res.ID, "importance": 5},
	} {
		var marks []string
		ctx := messaging.WithQuarantine(context.Background(), func(why string) error { marks = append(marks, why); return nil })
		b, _ := json.Marshal(args)
		out, failed := r.Call(ctx, env, "memory_manage", string(b))
		if failed || !strings.Contains(out, "balcony") {
			t.Fatalf("%v: %s", args, out)
		}
		if len(marks) == 0 || messaging.UntrustedWhy(ctx) == "" {
			t.Errorf("%v: the marked record's text came back unmarked: %s", args["action"], out)
		}
		// a mark that cannot be kept: no text
		ctx = messaging.WithQuarantine(context.Background(), func(string) error { return errors.New("disk full") })
		if out, failed := r.Call(ctx, env, "memory_manage", string(b)); !failed || strings.Contains(out, "balcony") {
			t.Errorf("%v: shown without a durable mark: %s", args["action"], out)
		}
	}
}
