package loop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
	"go.yaml.in/yaml/v3"
)

// Settings and schedules the agent changes at runtime: napped, proposed across a restart that
// reset them, decided, written to the repo, and a rejected runtime job dropped on the next boot.
func TestRuntimeSettingsAndSchedules(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "instance")
	example := "../../examples/minimal"
	for _, d := range []string{"units", "bridge", "agents/atlas"} {
		copyDir(t, filepath.Join(example, d), filepath.Join(root, d))
	}
	write(t, filepath.Join(root, "stormo.yaml"), read(t, filepath.Join(example, "stormo.yaml")))
	os.RemoveAll(filepath.Join(root, "agents", "atlas", "learnings"))
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	eng := hermes.New(inst)
	store := FsStore{Root: filepath.Join(tmp, "store")}
	scrubber := learning.NewScrubber(inst)
	agent := func() *manifest.Agent {
		a, err := manifest.Load(root, "atlas", inst.Names.Secret)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	n := 0
	baseline := func() string {
		n++
		r, err := build.Agent(inst, "atlas", build.Options{Out: filepath.Join(tmp, "baseline", string(rune('a'+n)))})
		if err != nil {
			t.Fatal(err)
		}
		return r.Out
	}
	boot := func(home, base string) {
		if _, err := Rehydrate(RehydrateOptions{Agent: agent(), Engine: eng, Home: home, BaselineDir: base, Store: store, Scrubber: scrubber}); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	nap := func(home, instanceID string) {
		at = at.Add(15 * time.Minute)
		if _, err := NapOnce(NapOptions{Agent: "atlas", Unit: "sales", Engine: eng, Home: home, Store: store, Instance: instanceID, Now: at}); err != nil {
			t.Fatal(err)
		}
	}
	editYAML := func(p string, fn func(map[string]any)) {
		var doc map[string]any
		if err := yaml.Unmarshal([]byte(read(t, p)), &doc); err != nil {
			t.Fatal(err)
		}
		fn(doc)
		b, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		write(t, p, string(b))
	}
	addJob := func(home, id string) {
		p := filepath.Join(home, "cron/jobs.json")
		var file struct {
			Jobs []map[string]any `json:"jobs"`
		}
		if err := json.Unmarshal([]byte(read(t, p)), &file); err != nil {
			t.Fatal(err)
		}
		job := map[string]any{}
		for k, v := range file.Jobs[0] {
			job[k] = v
		}
		job["id"], job["name"], job["enabled"] = id, "Runtime digest", true
		file.Jobs = append(file.Jobs, job)
		b, _ := json.MarshalIndent(file, "", "  ")
		write(t, p, string(b))
	}

	home1 := filepath.Join(tmp, "home1")
	boot(home1, baseline())
	var booted NapBaseline
	if err := json.Unmarshal([]byte(read(t, filepath.Join(home1, BootedPath))), &booted); err != nil || booted.Settings == nil {
		t.Fatalf("booted settings = %v, %v; want recorded (empty, not nil)", booted.Settings, err)
	}

	// At runtime: /sethome in another channel, a command approved "always", an MCP server with a
	// credential header, and a job of its own.
	editYAML(filepath.Join(home1, "config.yaml"), func(doc map[string]any) {
		doc["platforms"] = map[string]any{"slack": map[string]any{"home_channel": map[string]any{"platform": "slack", "chat_id": "C0000000099", "name": "ops"}}}
		doc["command_allowlist"] = []any{"git status"}
		doc["mcp_servers"] = map[string]any{"notion": map[string]any{"url": "https://mcp.example.com/notion", "headers": map[string]any{"Authorization": "Bearer abc"}}}
	})
	addJob(home1, "rt0001")
	nap(home1, "task-a")

	// A restart resets config.yaml; the runtime job comes back (rehydrate merges it).
	home2 := filepath.Join(tmp, "home2")
	boot(home2, baseline())
	if strings.Contains(read(t, filepath.Join(home2, "config.yaml")), "C0000000099") {
		t.Fatal("rehydrate restored the runtime config")
	}
	if !strings.Contains(read(t, filepath.Join(home2, "cron/jobs.json")), "rt0001") {
		t.Fatal("runtime job not restored")
	}
	write(t, filepath.Join(home2, "memories/MEMORY.md"), "Deals close on Fridays.")
	nap(home2, "task-b")
	// A config.yaml napped mid-write: its settings are skipped, the dream goes on.
	write(t, filepath.Join(home2, "config.yaml"), "platforms: {slack: [unclosed\n")
	write(t, filepath.Join(home2, "memories/MEMORY.md"), "Deals close on Fridays.\n§\nQuotes expire in 7 days.")
	nap(home2, "task-b")

	rep, err := Dream(inst, "atlas", store)
	if err != nil {
		t.Fatal(err)
	}
	if rep.SettingProposals != 3 || !rep.CronProposal {
		t.Fatalf("report = %+v; want 3 settings and a schedules proposal", rep)
	}
	props, err := ListSettingProposals(root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]SettingProposal{}
	for _, p := range props {
		byKey[p.Key] = p
	}
	if p := byKey["home_channel.slack"]; p.Value != "C0000000099" || p.Current != "C0000000001" || p.Sensitive {
		t.Errorf("home channel proposal = %+v", p)
	}
	if p := byKey["command_allowlist"]; !p.Sensitive || p.Value != `["git status"]` {
		t.Errorf("allowlist proposal = %+v", p)
	}
	if p := byKey["mcp_servers.notion"]; !p.Sensitive || strings.Contains(p.Value, "Bearer") {
		t.Errorf("mcp proposal = %+v; want sensitive, headers left out", p)
	}
	jobs, meta, err := ReadSchedulesProposal(root, "atlas")
	if err != nil || meta == nil || !slices.Equal(meta.RuntimeJobs, []string{"rt0001"}) || len(jobs) != 2 {
		t.Fatalf("schedules proposal = %v %+v %v", jobs, meta, err)
	}

	t.Run("accepting a home channel edits that channel in agent.yaml, comments kept", func(t *testing.T) {
		if _, err := DecideSetting(inst, "atlas", "home_channel.slack", "accepted", Decider{By: "core review (test-model)", Reason: "a channel id"}); err != nil {
			t.Fatal(err)
		}
		a := agent()
		if a.Channels[0].HomeChannel != "C0000000099" {
			t.Errorf("home channel = %q", a.Channels[0].HomeChannel)
		}
		if !strings.Contains(read(t, filepath.Join(root, "agents/atlas/agent.yaml")), "# Telegram runs only when TELEGRAM_BOT_TOKEN has a value") {
			t.Error("agent.yaml lost its comments")
		}
		wm, _ := LoadWatermark(root, "atlas")
		if d := wm.SettingDecisions["home_channel.slack"]; d.Decision != "accepted" || d.By == "" {
			t.Errorf("decision = %+v", d)
		}
	})

	t.Run("accepting an MCP server writes the engine config without credentials", func(t *testing.T) {
		if _, err := DecideSetting(inst, "atlas", "mcp_servers.notion", "accepted", Decider{}); err != nil {
			t.Fatal(err)
		}
		body := read(t, filepath.Join(root, "agents/atlas", hermes.ConfigFile))
		if !strings.Contains(body, "notion:") || !strings.Contains(body, "https://mcp.example.com/notion") || strings.Contains(body, "Bearer") {
			t.Errorf("engine config =\n%s", body)
		}
	})

	t.Run("rejected settings and schedules are not proposed again; the rejected job is dropped", func(t *testing.T) {
		if _, err := DecideSetting(inst, "atlas", "command_allowlist", "rejected", Decider{}); err != nil {
			t.Fatal(err)
		}
		if _, err := DecideSchedules(inst, "atlas", "rejected", Decider{}); err != nil {
			t.Fatal(err)
		}
		if exists(filepath.Join(root, "agents/atlas/learnings/proposals")) {
			t.Error("proposals left after every decision")
		}
		// The agent is still running with all of it (home1) and naps again.
		write(t, filepath.Join(home1, "memories/MEMORY.md"), "Quotes go out before noon.")
		nap(home1, "task-a")
		rep, err := Dream(inst, "atlas", store)
		if err != nil {
			t.Fatal(err)
		}
		if rep.SettingProposals != 0 || rep.CronProposal {
			t.Errorf("report = %+v; want nothing proposed again", rep)
		}
		home3 := filepath.Join(tmp, "home3")
		boot(home3, baseline())
		if strings.Contains(read(t, filepath.Join(home3, "cron/jobs.json")), "rt0001") {
			t.Error("the rejected runtime job came back")
		}
		if !strings.Contains(read(t, filepath.Join(home3, "cron/jobs.json")), "a1b2c3d4e5f6") {
			t.Error("the repo's job was dropped")
		}
	})
}
