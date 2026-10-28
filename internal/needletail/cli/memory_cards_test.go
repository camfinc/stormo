package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// Item 4 (Codex 4, Claude M5): a memory approval shows what it changes: the record's text (a
// diff for an update) and its revision, which the approval binds; the hot memory's change shows
// the entry it replaces or removes. A held report's retry shows the report.
func TestMemoryApprovalCards(t *testing.T) {
	h := home(t, "http://127.0.0.1:1/v1", "http://127.0.0.1:1/v1")
	mem := &memory.Store{Home: h, Limits: map[string]int{"memory": 2000, "user": 2000}}
	res, err := mem.Save(memory.SaveOptions{Text: "The CRM export runs on Fridays.", Kind: memory.KindFact, Src: "chat", Scope: memory.ScopeAgent, Untrusted: true, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.AddAs(memory.Audience{}, "chat", memory.TargetMemory, "Simon prefers Spanish."); err != nil {
		t.Fatal(err)
	}
	rt := &runtime{sched: sched.New(sched.Options{Home: h})}
	env := &tools.Env{Home: h, Sandbox: sandbox.Sandbox{Home: h}, Memory: mem}
	describe := func(tool string, args map[string]any) (string, string, map[string]string) {
		b, _ := json.Marshal(args)
		c, bind, err := rt.describeBound(env, tool, b)
		if err != nil {
			t.Fatal(err)
		}
		return c.Target + "\n" + c.Title, c.Payload, bind
	}
	_, rev, _ := mem.Record(memory.Audience{}, res.ID)
	if rev == "" {
		t.Fatal("no revision")
	}
	head, p, bind := describe("memory_manage", map[string]any{"action": "pin", "id": res.ID})
	if !strings.Contains(p, "The CRM export runs on Fridays.") || !strings.Contains(head, rev) || bind["record_revision"] != rev {
		t.Errorf("a pin card: %q %q %v", head, p, bind)
	}
	_, p, bind = describe("memory_manage", map[string]any{"action": "update", "id": res.ID, "text": "The CRM export runs on Mondays."})
	if !strings.Contains(p, "-The CRM export runs on Fridays.") || !strings.Contains(p, "+The CRM export runs on Mondays.") || bind["record_revision"] != rev {
		t.Errorf("an update card: %q %v", p, bind)
	}
	_, p, bind = describe("memory", map[string]any{"action": "replace", "target": "memory", "old_text": "Spanish", "content": "Simon prefers English."})
	if !strings.Contains(p, "-Simon prefers Spanish.") || !strings.Contains(p, "+Simon prefers English.") || bind["memory_entry"] != "Simon prefers Spanish." {
		t.Errorf("a hot memory change: %q %v", p, bind)
	}
}

// The approved change runs only on the record the card showed: a record changed since refuses
// it; the pin of a marked record endorses and pins it in one step.
func TestApprovedMemoryChangeBindsTheRevision(t *testing.T) {
	h := t.TempDir()
	mem := &memory.Store{Home: h, Limits: map[string]int{"memory": 2000, "user": 2000}}
	res, err := mem.Save(memory.SaveOptions{Text: "The CRM export runs on Fridays.", Kind: memory.KindFact, Src: "chat", Scope: memory.ScopeAgent, Untrusted: true, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	_, rev, _ := mem.Record(memory.Audience{}, res.ID)
	if _, err := mem.Update(memory.Audience{}, res.ID, memory.Change{Text: ptr("The CRM export runs on Sundays.")}, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mem.EndorsePin(memory.Audience{Revision: rev}, res.ID); err == nil {
		t.Fatal("a pin of a record changed since its card")
	}
	_, rev, _ = mem.Record(memory.Audience{}, res.ID)
	v, _, err := mem.EndorsePin(memory.Audience{Revision: rev}, res.ID)
	if err != nil || v.Hot == 0 {
		t.Fatalf("the approved pin: %+v %v", v, err)
	}
}

func ptr(s string) *string { return &s }
