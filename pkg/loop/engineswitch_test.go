package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/engine/hermes/hermestest"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
)

// stockEngine is the agent's engine under another kind, with the skills its image ships: an
// engine switch without naming a real engine.
type stockEngine struct {
	engine.Engine
	kind  string
	stock map[string]bool
}

func (e stockEngine) Kind() string                                          { return e.kind }
func (e stockEngine) EngineOwnedSkills(func(string) []byte) map[string]bool { return e.stock }

// An agent switches engines and its home carries the former image's skills over: the next boot
// must not restore them as the agent's, nor the dream propose them, while the agent's own skill
// still counts.
func TestEngineSwitchLeavesFormerImageSkills(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "instance")
	example := hermestest.Instance(t)
	for _, d := range []string{"units", "bridge", "agents/atlas"} {
		copyDir(t, filepath.Join(example, d), filepath.Join(root, d))
	}
	write(t, filepath.Join(root, "stormo.yaml"), read(t, filepath.Join(example, "stormo.yaml")))
	os.RemoveAll(filepath.Join(root, "agents", "atlas", "learnings"))
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	old := stockEngine{hermes.New(inst), "old", map[string]bool{"airtable": true}}
	cur := stockEngine{hermes.New(inst), "new", map[string]bool{}}
	resolve := func(kind string) (engine.Engine, error) {
		switch kind {
		case "old":
			return old, nil
		case "new":
			return cur, nil
		}
		return nil, fmt.Errorf("unknown engine %q", kind)
	}
	get := engineFor
	engineFor = func(kind string, _ *instance.Instance) (engine.Engine, error) {
		if kind == "old" {
			return old, nil
		}
		return cur, nil // the agent's manifest now names the new engine
	}
	t.Cleanup(func() { engineFor = get })

	store := FsStore{Root: filepath.Join(tmp, "store")}
	scrubber := learning.NewScrubber(inst)
	agent, err := manifest.Load(root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	b, err := build.Agent(inst, "atlas", build.Options{Out: filepath.Join(tmp, "baseline")})
	if err != nil {
		t.Fatal(err)
	}

	// Under the old engine: its image seeds airtable, the agent writes order-lookup, a nap.
	home := filepath.Join(tmp, "home1")
	if _, err := Rehydrate(RehydrateOptions{Agent: agent, Engine: old, Home: home, BaselineDir: b.Out, Store: store, Scrubber: scrubber}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "skills/productivity/airtable/SKILL.md"), "---\nname: airtable\ndescription: the image's\n---\nbody\n")
	write(t, filepath.Join(home, "skills/crm/order-lookup/SKILL.md"), "---\nname: order-lookup\ndescription: find a deal by order id\n---\nAsk for the order id first.\n")
	if m, err := NapOnce(NapOptions{Agent: "atlas", Unit: agent.Unit, Engine: old, Home: home, Store: store, Instance: "task-a", BaselineDir: b.Out, Now: time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)}); err != nil || m == nil {
		t.Fatalf("nap: %v %v", m, err)
	}

	t.Run("the dream proposes the agent's skill, not the former image's", func(t *testing.T) {
		r, err := Dream(inst, "atlas", store)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, p := range r.SkillProposals {
			got[p.Skill] = true
		}
		if got["productivity/airtable"] || !got["crm/order-lookup"] {
			t.Errorf("skill proposals = %v", r.SkillProposals)
		}
	})

	t.Run("the first boot on the new engine leaves the former image's skill behind", func(t *testing.T) {
		home2 := filepath.Join(tmp, "home2")
		r, err := Rehydrate(RehydrateOptions{Agent: agent, Engine: cur, Home: home2, BaselineDir: b.Out, Store: store, Scrubber: scrubber, Engines: resolve})
		if err != nil {
			t.Fatal(err)
		}
		if exists(filepath.Join(home2, "skills/productivity/airtable")) {
			t.Errorf("former image's skill restored: %v", r.RestoredSkills)
		}
		if !exists(filepath.Join(home2, "skills/crm/order-lookup/SKILL.md")) {
			t.Errorf("agent's skill not restored: %v", r.RestoredSkills)
		}
	})
}
