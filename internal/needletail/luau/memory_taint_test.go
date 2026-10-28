package luau

import (
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/memory"
)

// Item 13 (Codex 2 and 3, Claude M1): a record saved from untrusted input carries its mark into
// whatever shows its text to a script: search_memory, a save's "similar" records, an update's
// echo. The run is marked before the text is returned (no mark, no data).
func TestLuauMemoryResultsCarryTheMark(t *testing.T) {
	for _, c := range []struct{ name, src string }{
		{"search", `return search_memory("balcony")`},
		{"similar", `return save_memory("Ana prefers balcony cabins on cruises!", {})`},
		{"update echo", `return update_memory(ID, {importance = 5})`},
	} {
		t.Run(c.name, func(t *testing.T) {
			rt, _ := newRuntime(t)
			rt.Grants = append(rt.Grants, "memory:read", "memory:write")
			res0, err := rt.Memory.Save(memory.SaveOptions{Text: "Ana prefers balcony cabins on cruises.", Kind: memory.KindPreference, Src: "chat", Scope: memory.ScopeAgent, Untrusted: true, Force: true})
			if err != nil {
				t.Fatal(err)
			}
			src := strings.ReplaceAll(c.src, "ID", `"`+res0.ID+`"`)
			res, _ := exec(t, rt, []string{"memory:read", "memory:write"}, src)
			if res.Err != nil {
				t.Fatalf("run: %v", res.Err)
			}
			if !strings.Contains(string(res.Value), "balcony") {
				t.Fatalf("the record's text was not returned: %s", res.Value)
			}
			if res.Untrusted == "" {
				t.Errorf("a marked record's text reached the script without marking the run: %s", res.Value)
			}
		})
	}
}
