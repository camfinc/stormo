package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Capabilities are declared, changed and removed one stanza at a time: the rest of stormo.yaml stays
// as written, a stale hash or a file that would not load is refused before anything is written, and
// an agent is granted only what is declared and can run.
func TestCapabilityEdits(t *testing.T) {
	inst := acme(t)
	path := filepath.Join(inst.Root, "stormo.yaml")
	before, _ := os.ReadFile(path)
	f, err := ShowCapabilities(inst)
	if err != nil || len(f.Capabilities) != 0 {
		t.Fatalf("show: %+v %v", f, err)
	}

	// declare one from a template: the file gains only the capabilities block, comments kept
	f, err = SetCapability(inst, "audio.transcribe", []byte(`{"template":"transcribe","idle_minutes":5}`), f.Hash)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(after), strings.TrimRight(string(before), "\n")) || !strings.Contains(string(after), "audio.transcribe:") {
		t.Errorf("the rest of stormo.yaml changed:\n%s", after)
	}
	c := f.Capabilities[0]
	if c.Name != "audio.transcribe" || c.Spec["template"] != "transcribe" || c.Effective.IdleMinutes != 5 || c.Effective.Kind != "service" || len(c.Agents) != 0 {
		t.Errorf("declared: %+v", c)
	}
	// change one setting, remove another (null)
	if f, err = SetCapability(inst, "audio.transcribe", []byte(`{"concurrency":2,"idle_minutes":null}`), f.Hash); err != nil {
		t.Fatal(err)
	}
	if c := f.Capabilities[0]; c.Effective.Concurrency != 2 || c.Effective.IdleMinutes != 10 || c.Spec["idle_minutes"] != nil {
		t.Errorf("changed: %+v", c)
	}

	// refused, nothing written: a stale hash, a template this engine does not ship, a bad setting, a bad name
	now, _ := os.ReadFile(path)
	for patch, want := range map[string]string{
		`{"concurrency":3}`:        "conflict",
		`{"template":"nope"}`:      "invalid",
		`{"concurrency":"many"}`:   "invalid",
		`{"timeout_minutes":9999}`: "invalid",
	} {
		hash := f.Hash
		if want == "conflict" {
			hash = "stale"
		}
		if _, err := SetCapability(inst, "audio.transcribe", []byte(patch), hash); code(err) != want {
			t.Errorf("%s: %v, want %s", patch, err, want)
		}
	}
	if _, err := SetCapability(inst, "Bad Name", []byte(`{}`), f.Hash); code(err) != "invalid" {
		t.Errorf("bad name: %v", err)
	}
	if again, _ := os.ReadFile(path); string(again) != string(now) {
		t.Error("a refused change was written")
	}

	// grants: only declared, runnable capabilities
	agent := "agents/atlas/agent.yaml"
	shown, _ := Show(inst, agent)
	if _, err := Apply(inst, agent, []byte(`{"capabilities":["video.render"]}`), shown.Hash); code(err) != "invalid" {
		t.Errorf("an undeclared grant: %v", err)
	}
	if _, err := Apply(inst, agent, []byte(`{"capabilities":["audio.transcribe"]}`), shown.Hash); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if o := OptionsFor(inst, "atlas"); len(o.Capabilities) != 1 || o.Capabilities[0].Name != "audio.transcribe" {
		t.Errorf("form options: %+v", o.Capabilities)
	}
	f, _ = ShowCapabilities(inst)
	if strings.Join(f.Capabilities[0].Agents, ",") != "atlas" {
		t.Errorf("grants: %+v", f.Capabilities[0].Agents)
	}

	// removal: refused while granted, then done
	if _, err := RemoveCapability(inst, "audio.transcribe", f.Hash); code(err) != "in_use" {
		t.Errorf("remove while granted: %v", err)
	}
	shown, _ = Show(inst, agent)
	if _, err := Apply(inst, agent, []byte(`{"capabilities":null}`), shown.Hash); err != nil {
		t.Fatal(err)
	}
	if f, err = RemoveCapability(inst, "audio.transcribe", f.Hash); err != nil || len(f.Capabilities) != 0 {
		t.Fatalf("remove: %+v %v", f, err)
	}
	if _, err := RemoveCapability(inst, "audio.transcribe", f.Hash); code(err) != "not_found" {
		t.Errorf("remove twice: %v", err)
	}
}
