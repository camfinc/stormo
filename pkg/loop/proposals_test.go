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
	"github.com/camfinc/stormo/pkg/engine/hermes/hermestest"
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

// Scripts the agent writes or edits for its own jobs (subdirectories included, caches not): napped,
// restored across restarts until reviewed, proposed with the schedules, accepted into
// agents/<id>/scripts/ or rejected and dropped. A script the repo deletes while the agent runs is
// neither proposed nor restored; an edit to one the repo changed since is stale.
func TestRuntimeScripts(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "instance")
	example := hermestest.Instance(t)
	for _, d := range []string{"units", "bridge", "agents/atlas"} {
		copyDir(t, filepath.Join(example, d), filepath.Join(root, d))
	}
	write(t, filepath.Join(root, "stormo.yaml"), read(t, filepath.Join(example, "stormo.yaml")))
	os.RemoveAll(filepath.Join(root, "agents", "atlas", "learnings"))
	repoScripts := filepath.Join(root, "agents", "atlas", "scripts")
	write(t, filepath.Join(repoScripts, "report.py"), "print('report')\n")
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	eng := hermes.New(inst)
	store := FsStore{Root: filepath.Join(tmp, "store")}
	scrubber := learning.NewScrubber(inst)
	n := 0
	boot := func(name string) (string, *RehydrateReport) {
		n++
		r, err := build.Agent(inst, "atlas", build.Options{Out: filepath.Join(tmp, "baseline", string(rune('a'+n)))})
		if err != nil {
			t.Fatal(err)
		}
		a, err := manifest.Load(root, "atlas", inst.Names.Secret)
		if err != nil {
			t.Fatal(err)
		}
		home := filepath.Join(tmp, name)
		rep, err := Rehydrate(RehydrateOptions{Agent: a, Engine: eng, Home: home, BaselineDir: r.Out, Store: store, Scrubber: scrubber})
		if err != nil {
			t.Fatal(err)
		}
		return home, rep
	}
	at := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	nap := func(home string) {
		at = at.Add(15 * time.Minute)
		if _, err := NapOnce(NapOptions{Agent: "atlas", Unit: "sales", Engine: eng, Home: home, Store: store, Instance: "task", Now: at}); err != nil {
			t.Fatal(err)
		}
	}
	// addJob adds a runtime job whose monitor is script (Hermes' cron/jobs.json).
	addJob := func(home, id, script string) {
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
		job["id"], job["name"], job["enabled"], job["monitor_script"] = id, "Watch "+id, true, script
		file.Jobs = append(file.Jobs, job)
		b, _ := json.MarshalIndent(file, "", "  ")
		write(t, p, string(b))
	}
	proposal := func() *SchedulesProposal {
		t.Helper()
		if _, err := Dream(inst, "atlas", store); err != nil {
			t.Fatal(err)
		}
		_, meta, err := ReadSchedulesProposal(root, "atlas")
		if err != nil {
			t.Fatal(err)
		}
		return meta
	}
	names := func(meta *SchedulesProposal) []string {
		out := []string{}
		if meta != nil {
			for _, c := range meta.Scripts {
				out = append(out, c.Name+":"+c.Status)
			}
		}
		return out
	}

	home1, _ := boot("home1")
	var booted NapBaseline
	if err := json.Unmarshal([]byte(read(t, filepath.Join(home1, BootedPath))), &booted); err != nil ||
		booted.Scripts["report.py"] != build.Sha256([]byte("print('report')\n")) || len(booted.Scripts) != 1 {
		t.Fatalf("booted scripts = %v, %v", booted.Scripts, err)
	}

	// At runtime the agent splits its job: a monitor, helpers it imports (one in a package), an edit
	// to the repo's script, and a job that runs the monitor. Python leaves its cache behind.
	write(t, filepath.Join(home1, "scripts/watch.py"), "import helper\nhelper.run()\n")
	write(t, filepath.Join(home1, "scripts/helper.py"), "def run():\n    print('ok')\n")
	write(t, filepath.Join(home1, "scripts/lib/util.py"), "X = 1\n")
	write(t, filepath.Join(home1, "scripts/__pycache__/helper.cpython-312.pyc"), "cache")
	write(t, filepath.Join(home1, "scripts/report.py"), "print('report v2')\n")
	addJob(home1, "rt0001", "watch.py")
	nap(home1)
	meta := proposal()
	if got := names(meta); !slices.Equal(got, []string{"helper.py:new", "lib/util.py:new", "report.py:changed", "watch.py:new"}) {
		t.Fatalf("proposed scripts = %v", got)
	}
	if text := read(t, filepath.Join(SchedulesScriptsDir(root, "atlas"), "watch.py")); text != "import helper\nhelper.run()\n" {
		t.Errorf("proposed watch.py = %q", text)
	}
	a, _ := manifest.Load(root, "atlas", inst.Names.Secret)
	jobs, _, _ := ReadSchedulesProposal(root, "atlas")
	if _, held := schedulesCandidate(a, jobs, meta); held == "" {
		t.Error("a proposal carrying scripts must wait for a person")
	}

	// A restart before anyone reviewed: everything the agent wrote or edited comes back, the cache
	// does not.
	home2, rep := boot("home2")
	if !slices.Equal(rep.RestoredScripts, []string{"helper.py", "lib/util.py", "report.py", "watch.py"}) {
		t.Errorf("restored scripts = %v", rep.RestoredScripts)
	}
	if got := read(t, filepath.Join(home2, "scripts/report.py")); got != "print('report v2')\n" {
		t.Errorf("report.py = %q; the agent's edit stays until reviewed", got)
	}
	if exists(filepath.Join(home2, "scripts/__pycache__")) {
		t.Error("python cache restored")
	}
	if !strings.Contains(read(t, filepath.Join(home2, "cron/jobs.json")), `"watch.py"`) {
		t.Error("the runtime job lost its monitor")
	}
	nap(home2)
	if got := names(proposal()); !slices.Equal(got, []string{"helper.py:new", "lib/util.py:new", "report.py:changed", "watch.py:new"}) {
		t.Fatalf("proposed scripts after the restart = %v", got)
	}

	t.Run("an accept the manifest refuses puts the scripts back", func(t *testing.T) {
		dir := proposalsRoot(root, "atlas")
		saved := read(t, filepath.Join(dir, "schedules.yaml"))
		write(t, filepath.Join(dir, "schedules.yaml"), strings.Replace(saved, "watch.py", "missing.py", 1))
		if _, err := DecideSchedules(inst, "atlas", "accepted", Decider{}); err == nil || !strings.Contains(err.Error(), "missing.py") {
			t.Fatalf("accept = %v; want refused for missing.py", err)
		}
		for _, f := range []string{"watch.py", "helper.py", "lib/util.py"} {
			if exists(filepath.Join(repoScripts, f)) {
				t.Errorf("%s left in the repo after a refused accept", f)
			}
		}
		if got := read(t, filepath.Join(repoScripts, "report.py")); got != "print('report')\n" {
			t.Errorf("repo report.py = %q; a refused accept must put it back", got)
		}
		write(t, filepath.Join(dir, "schedules.yaml"), saved)
	})

	t.Run("accept writes the scripts with the schedules; nothing is proposed after", func(t *testing.T) {
		if _, err := DecideSchedules(inst, "atlas", "accepted", Decider{}); err != nil {
			t.Fatal(err)
		}
		for f, want := range map[string]string{"watch.py": "import helper\nhelper.run()\n", "lib/util.py": "X = 1\n", "report.py": "print('report v2')\n"} {
			if got := read(t, filepath.Join(repoScripts, f)); got != want {
				t.Errorf("repo %s = %q", f, got)
			}
		}
		a, err := manifest.Load(root, "atlas", inst.Names.Secret)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(a.Schedules, func(s manifest.Schedule) bool { return s.ID == "rt0001" }); i < 0 || a.Schedules[i].Monitor != "watch.py" {
			t.Errorf("agent.yaml schedules = %+v", a.Schedules)
		}
		if exists(SchedulesScriptsDir(root, "atlas")) {
			t.Error("proposal scripts left after the decision")
		}
		home3, rep := boot("home3")
		if len(rep.RestoredScripts) != 0 {
			t.Errorf("restored %v; the repo has them now", rep.RestoredScripts)
		}
		nap(home3)
		if meta := proposal(); meta != nil {
			t.Errorf("proposal after accept = %+v", meta)
		}

		// The repo then drops report.py while home3 still runs it: not the agent's, so neither
		// proposed nor restored.
		if err := os.Remove(filepath.Join(repoScripts, "report.py")); err != nil {
			t.Fatal(err)
		}
		if meta := proposal(); meta != nil {
			t.Errorf("a script the repo deleted was proposed: %+v", meta)
		}
		home4, _ := boot("home4")
		if exists(filepath.Join(home4, "scripts/report.py")) {
			t.Error("a script the repo deleted came back")
		}

		t.Run("a rejected script and its job are dropped at the next start", func(t *testing.T) {
			write(t, filepath.Join(home4, "scripts/scrape.py"), "print('scrape')\n")
			addJob(home4, "rt0002", "scrape.py")
			nap(home4)
			if got := names(proposal()); !slices.Equal(got, []string{"scrape.py:new"}) {
				t.Fatalf("proposed = %v", got)
			}
			if _, err := DecideSchedules(inst, "atlas", "rejected", Decider{}); err != nil {
				t.Fatal(err)
			}
			wm, _ := LoadWatermark(root, "atlas")
			if wm.RejectedScripts["scrape.py"] != build.Sha256([]byte("print('scrape')\n")) {
				t.Errorf("rejected scripts = %v", wm.RejectedScripts)
			}
			if meta := proposal(); meta != nil {
				t.Errorf("rejected script proposed again: %+v", meta)
			}
			home5, _ := boot("home5")
			if exists(filepath.Join(home5, "scripts/scrape.py")) || strings.Contains(read(t, filepath.Join(home5, "cron/jobs.json")), "rt0002") {
				t.Error("rejected script or job restored")
			}

			t.Run("an edit to a script the repo changed since is stale: refused, and the repo wins", func(t *testing.T) {
				write(t, filepath.Join(home5, "scripts/watch.py"), "print('agent edit')\n")
				nap(home5)
				write(t, filepath.Join(repoScripts, "watch.py"), "print('repo edit')\n")
				meta := proposal()
				if meta == nil || len(meta.Scripts) != 1 || !meta.Scripts[0].Stale {
					t.Fatalf("proposal = %+v; want watch.py stale", meta)
				}
				if _, err := DecideSchedules(inst, "atlas", "accepted", Decider{}); err == nil || !strings.Contains(err.Error(), "watch.py") {
					t.Fatalf("accept = %v; want refused", err)
				}
				if got := read(t, filepath.Join(repoScripts, "watch.py")); got != "print('repo edit')\n" {
					t.Errorf("repo watch.py = %q; a refused accept changed it", got)
				}
				home6, rep := boot("home6")
				if !slices.Equal(rep.KeptRepoScripts, []string{"watch.py"}) || read(t, filepath.Join(home6, "scripts/watch.py")) != "print('repo edit')\n" {
					t.Errorf("kept %v, watch.py = %q; want the repo's newer version", rep.KeptRepoScripts, read(t, filepath.Join(home6, "scripts/watch.py")))
				}
			})
		})
	})
}
