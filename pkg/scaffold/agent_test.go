package scaffold

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

func newInstance(t *testing.T, template string) *instance.Instance {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	if _, err := NewInstance(dir, Options{Name: "Demo", Template: template}, exampleFS(t)); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// What `stormo check` does for one agent: it loads, and the engine compiles it.
func checkAgent(t *testing.T, inst *instance.Instance, id string) *manifest.Agent {
	t.Helper()
	a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := build.Agent(inst, id, build.Options{Out: filepath.Join(t.TempDir(), id)}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewAgentFromDefaults(t *testing.T) {
	inst := newInstance(t, Example)
	d := Defaults(inst)
	if d.Unit == "" || d.Unit == "group" || d.Engine.Version == "" || d.Model.Name == "" {
		t.Fatalf("defaults %+v", d)
	}
	r, err := NewAgent(inst, AgentSpec{Name: "Front Desk", Role: "Greets visitors.", Channels: []ChannelSpec{{Kind: "slack", AllowBots: "mentions"}}})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "front-desk" || r.Unit != d.Unit {
		t.Errorf("%+v", r)
	}
	for _, n := range []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "OPENROUTER_API_KEY"} {
		if !slices.Contains(r.Secrets, n) {
			t.Errorf("secrets %v lack %s", r.Secrets, n)
		}
	}
	if !slices.Contains(r.Missing, "SLACK_BOT_TOKEN") || slices.Contains(r.Missing, "API_SERVER_KEY") {
		t.Errorf("missing %v: want the channel's tokens, not the engine key secrets init mints", r.Missing)
	}
	a := checkAgent(t, inst, "front-desk")
	if a.Format != manifest.Format || a.Engine.Version != d.Engine.Version || a.Role != "Greets visitors." {
		t.Errorf("%+v", a)
	}
	soul, _ := os.ReadFile(filepath.Join(inst.Root, "agents", "front-desk", "SOUL.md"))
	if !strings.HasPrefix(string(soul), "# Front Desk\n\nGreets visitors.") {
		t.Errorf("SOUL.md %q", soul)
	}
	body, _ := os.ReadFile(filepath.Join(inst.Root, "agents", "front-desk", "agent.yaml"))
	if !strings.Contains(string(body), "# Names only") {
		t.Errorf("agent.yaml lost its comments:\n%s", body)
	}
}

func TestNewAgentInANewUnit(t *testing.T) {
	inst := newInstance(t, Empty)
	if _, err := NewAgent(inst, AgentSpec{Name: "Ada"}); err == nil {
		t.Error("an empty instance has no unit and no engine version to borrow; want an error")
	}
	spec := AgentSpec{
		Name:    "Ada",
		NewUnit: &manifest.Unit{ID: "ops", Name: "Operations", Description: "Back office."},
		Engine:  EngineChoice{Kind: Defaults(inst).Engine.Kind, Version: "0.21.5", ImageTag: "v2026.9.24"},
		Model:   ModelChoice{Name: "openai/gpt-6-luna", LocalName: "gpt-5.6-luna"},
		Soul:    "# Ada\n\nYou run operations.",
	}
	r, err := NewAgent(inst, spec)
	if err != nil {
		t.Fatal(err)
	}
	if r.Unit != "ops" || !slices.Contains(r.Files, "units/ops/unit.yaml") {
		t.Errorf("%+v", r)
	}
	a := checkAgent(t, inst, "ada")
	if a.Engine.Local == nil || a.Engine.Local.Model != "gpt-5.6-luna" {
		t.Errorf("local %+v", a.Engine.Local)
	}
}

func TestNewAgentRefusals(t *testing.T) {
	inst := newInstance(t, Example)
	cases := map[string]AgentSpec{
		"no name":       {},
		"taken id":      {Name: "Atlas"},
		"bad id":        {Name: "X", ID: "X!"},
		"unknown unit":  {Name: "X", Unit: "nowhere"},
		"group":         {Name: "X", Unit: "group"},
		"unit exists":   {Name: "X", NewUnit: &manifest.Unit{ID: "sales"}},
		"unknown kind":  {Name: "X", Channels: []ChannelSpec{{Kind: "fax"}}},
		"no connection": {Name: "X", Model: ModelChoice{Provider: "nowhere"}},
	}
	before := manifest.AgentIDs(inst.Root)
	units := manifest.UnitIDs(inst.Root)
	for name, s := range cases {
		if _, err := NewAgent(inst, s); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if got := manifest.AgentIDs(inst.Root); !slices.Equal(got, before) {
		t.Errorf("a refused agent was left behind: %v", got)
	}
	if got := manifest.UnitIDs(inst.Root); !slices.Equal(got, units) {
		t.Errorf("a refused unit was left behind: %v", got)
	}
}

func TestNewAgentOptions(t *testing.T) {
	inst := newInstance(t, Example)
	o := NewAgentOptions(inst)
	if len(o.Options.Units) < 2 || len(o.Options.Channels) == 0 || len(o.Options.Connections) == 0 || !slices.Contains(o.TakenIDs, "atlas") {
		t.Errorf("%+v", o)
	}
}
