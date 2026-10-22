package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/luau"
)

var updateGolden = flag.Bool("update", false, "rewrite testdata goldens")

// The guide section is generated (the home's layout, the Luau registry, the agent's grants, the
// messaging rules) and pinned here; it stays within MaxGuide, since it is in every turn.
func TestGuideGolden(t *testing.T) {
	full := guide{Home: "/opt/data", Shared: []string{"/shared/group", "/shared/sales"}, Split: true, Luau: true,
		Grants: []string{"fs.read:home", "fs.write:home", "core:msg_*", "http:api.example.com", "message:slack"}, LuauTimeout: 30, ScriptTimeout: 900,
		Slack: true, Broadcast: false}
	got := full.render()
	golden := filepath.Join("testdata", "guide.golden")
	if *updateGolden {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the guide changed (go test -run TestGuideGolden -update to accept):\n%s", got)
	}
	if len(got) > MaxGuide {
		t.Errorf("the guide is %d bytes, over %d", len(got), MaxGuide)
	}
	for _, b := range luau.Bindings() {
		name := b.Global
		if name == "" {
			name = "nt." + b.Name
		}
		if !strings.Contains(got, "`"+name) {
			t.Errorf("the guide lacks %s", name)
		}
	}
	for _, e := range luau.EventNames {
		if !strings.Contains(got, "`"+e+"`") {
			t.Errorf("the guide lacks %s", e)
		}
	}
	// what this runtime does not run is not described
	min := guide{Home: "/opt/data"}.render()
	for _, absent := range []string{"Luau", "send_message", "own user"} {
		if strings.Contains(min, absent) {
			t.Errorf("a guide without Luau, Slack or the uid split mentions %q:\n%s", absent, min)
		}
	}
	t.Logf("guide: %d bytes in full, %d without Luau and Slack", len(got), len(min))
	// the two bounds are told apart: a tool's and a message handler's, an on_schedule handler's
	if !strings.Contains(got, "30 s for tools and the other handlers (luau_timeout)") || !strings.Contains(got, "900 s for on_schedule handlers and .luau job scripts (script_timeout)") {
		t.Error("the guide does not tell luau_timeout from script_timeout")
	}
}

// MaxGuide holds in render, not only in this test: a reference that grew past it is cut at a
// line, with a pointer to luau_reference, and the messaging rules after it stay whole.
func TestGuideStaysWithinMaxGuide(t *testing.T) {
	was := compactReference
	t.Cleanup(func() { compactReference = was })
	compactReference = func() string { return strings.Repeat("- `nt.fn(x)`: a function with a long description.\n", 600) }
	g := guide{Home: "/opt/data", Luau: true, LuauTimeout: 30, ScriptTimeout: 30, Slack: true}
	got := g.render()
	if len(got) > MaxGuide {
		t.Fatalf("the guide is %d bytes, over %d", len(got), MaxGuide)
	}
	if !strings.Contains(got, "luau_reference has the rest") || !strings.Contains(got, "## Messages\n") ||
		!strings.HasSuffix(got, "A scheduled run's final reply is its delivery: do not send it yourself.\n\n") {
		t.Errorf("the guide:\n%s", got)
	}
	if i := strings.Index(got, "…: luau_reference has the rest"); i < 1 || got[i-1] != '\n' {
		t.Error("the reference was not cut at a line")
	}
}

// boot puts the guide in the system prompt, after the skills and before the current time, and
// registers luau_reference for the full form.
func TestGuideInSystemPrompt(t *testing.T) {
	h := home(t, "http://127.0.0.1:1/v1", "http://127.0.0.1:1/v1")
	setenv(t, h)
	rt, err := boot(h, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	p := rt.agent.SystemPrompt()
	i, j, k := strings.Index(p, "## Your home\n"), strings.Index(p, "## Luau tools and handlers\n"), strings.Index(p, "## Now\n")
	if i < 0 || j < i || k < j || strings.Contains(p, "## Messages\n") {
		t.Errorf("system prompt:\n%s", p)
	}
	if _, ok := rt.agent.Tools.Get("luau_reference"); !ok {
		t.Error("no luau_reference tool")
	}
}

// The guide's variable parts are budgeted too: many grants are summarised past a share of the
// guide (with where to see the rest), and even very long paths never take it over MaxGuide.
func TestGuideBudgetsItsVariableParts(t *testing.T) {
	grants := []string{}
	for i := 0; i < 500; i++ {
		grants = append(grants, fmt.Sprintf("http:api-%03d.internal.example.com", i))
	}
	g := guide{Home: "/opt/data", Shared: []string{"/shared/group", "/shared/sales"}, Luau: true, Grants: grants, LuauTimeout: 30, ScriptTimeout: 900, Slack: true}
	got := g.render()
	if len(got) > MaxGuide {
		t.Fatalf("the guide is %d bytes with 500 grants, over %d", len(got), MaxGuide)
	}
	if !strings.Contains(got, "http:api-000.internal.example.com") || !strings.Contains(got, "more (") || !strings.HasSuffix(got, "do not send it yourself.\n\n") {
		t.Errorf("the guide:\n%s", got)
	}
	g.Home = "/" + strings.Repeat("h", 9000)
	g.Shared = []string{"/" + strings.Repeat("g", 9000), "/" + strings.Repeat("u", 9000)}
	if got := g.render(); len(got) > MaxGuide {
		t.Fatalf("the guide is %d bytes with long paths, over %d", len(got), MaxGuide)
	}
}
