package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	nloop "github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/build"
	adapter "github.com/camfinc/stormo/pkg/engine/needletail"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
)

// oneTurn is a scheduler's turn slots for a test: each turn answers reply.
type oneTurn struct {
	mu     sync.Mutex
	reply  string
	inputs []string
}

func (o *oneTurn) Reserve(session string, _ wire.Session) (sched.Slot, error) {
	return &oneSlot{o, session}, nil
}

type oneSlot struct {
	o       *oneTurn
	session string
}

func (s *oneSlot) Turn(_ context.Context, input string, _ nloop.Options) (*nloop.Result, error) {
	s.o.mu.Lock()
	defer s.o.mu.Unlock()
	s.o.inputs = append(s.o.inputs, input)
	return &nloop.Result{Text: s.o.reply, Session: s.session}, nil
}
func (s *oneSlot) Release() {}

func jobsIn(t *testing.T, home string) map[string]map[string]any {
	t.Helper()
	var f struct{ Jobs []map[string]any }
	readJSON(t, filepath.Join(home, "cron", "jobs.json"), &f)
	out := map[string]map[string]any{}
	for _, j := range f.Jobs {
		out[fmt.Sprint(j["id"])] = j
	}
	return out
}

// M4's done-when, last part (docs/needletail.md §9): an agent-made job survives a rehydrate.
// The real build of the example atlas agent on needletail; the agent makes a job with
// cronjob_manage in a turn (and edits the repo's job); the job runs; nap, dream, rehydrate into
// a new home: the agent's job is back with its run state and runs again, the repo's job is back
// with the repo's definition, and the dream proposes the agent's job for review.
func TestAgentJobSurvivesRehydrate(t *testing.T) {
	model := &scripted{}
	srv := httptest.NewServer(model)
	defer srv.Close()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "instance")
	if err := os.CopyFS(root, os.DirFS(filepath.Join("..", "..", "..", "examples", "minimal"))); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(root, "agents", "atlas", "learnings"))
	manifestPath := filepath.Join(root, "agents", "atlas", "agent.yaml")
	raw, _ := os.ReadFile(manifestPath)
	m := strings.Replace(string(raw), "  provider: openrouter\n", "  provider: stub\n", 1)
	if err := os.WriteFile(manifestPath, []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(filepath.Join(root, "stormo.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	fmt.Fprintf(f, "\nconnections:\n  - {name: stub, kind: custom, base_url: %s/v1, key: OPENROUTER_API_KEY}\n", srv.URL)
	f.Close()
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manifest.Load(root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	eng := adapter.New(inst)
	base := filepath.Join(tmp, "baseline")
	if _, err := build.Agent(inst, "atlas", build.Options{Out: base, Target: manifest.AWS}); err != nil {
		t.Fatal(err)
	}
	store := loop.FsStore{Root: filepath.Join(tmp, "store")}
	scrubber := learning.NewScrubber(inst)
	rehydrate := func(home string) *loop.RehydrateReport {
		t.Helper()
		r, err := loop.Rehydrate(loop.RehydrateOptions{Agent: agent, Engine: eng, Home: home, BaselineDir: base, Store: store, Scrubber: scrubber})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	repoJob := agent.Schedules[0]

	// 1. First boot; in a turn the agent writes a script for a job of its own (with a module in a
	// subdirectory it sources), schedules the job to run it, and tries to rewrite the repo job
	// (refused: what a repo job runs is agent.yaml's).
	home1 := filepath.Join(tmp, "home1")
	rehydrate(home1)
	const sweep = "source lib/deals.sh\necho \"stale deals: $(stale_deals)\"\n"
	const deals = "stale_deals() { echo ACME-1 ACME-2; }\n"
	model.script(
		toolStep("c0", "write_file", map[string]any{"path": "scripts/stale_sweep.sh", "content": sweep}),
		toolStep("c0b", "write_file", map[string]any{"path": "scripts/lib/deals.sh", "content": deals}),
		toolStep("c1", "cronjob_manage", map[string]any{"action": "create", "name": "Stale deal sweep", "schedule": "every 2h",
			"prompt": "Sweep deals with no activity in 3 days and report them.", "skills": []string{"crm-api"}, "script": "stale_sweep.sh",
			"enabled_toolsets": []string{"terminal", "skills"}, "deliver": "slack"}),
		toolStep("c2", "cronjob_manage", map[string]any{"action": "update", "job_id": repoJob.ID, "prompt": "Only list the deals."}),
		toolStep("c3", "cronjob_manage", map[string]any{"action": "list"}),
		textStep("Done."),
	)
	setenv(t, home1)
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"oneshot", "--home", home1, "--format", "stream-json", "-q", "Sweep stale deals every two hours."}, &stdout, &stderr)
	if run := eng.ParseBench(stdout.String(), code); code != 0 || run.Text != "Done." ||
		!slices.Equal(run.Tools, []string{"write_file", "write_file", "cronjob_manage", "cronjob_manage", "cronjob_manage"}) {
		t.Fatalf("oneshot exit %d: %+v\nstderr: %s", code, run, stderr.String())
	}
	var mine string
	for id, j := range jobsIn(t, home1) {
		if j["name"] == "Stale deal sweep" {
			mine = id
		}
	}
	if mine == "" {
		t.Fatalf("no agent job in %v", jobsIn(t, home1))
	}
	if got := jobsIn(t, home1)[repoJob.ID]["prompt"]; got != repoJob.Prompt {
		t.Errorf("the agent rewrote the repo job's prompt: %v", got)
	}

	// 2. It runs once (the scheduler as serve runs it), two hours on.
	t0 := time.Now().UTC().Truncate(time.Second)
	turns := &oneTurn{reply: "Two stale deals: ACME-1, ACME-2."}
	clock := t0.Add(2*time.Hour + time.Minute)
	s1 := sched.New(sched.Options{Home: home1, Turns: turns, Now: func() time.Time { return clock }})
	if err := s1.Boot(); err != nil {
		t.Fatal(err)
	}
	s1.Tick()
	s1.Wait()
	before := jobsIn(t, home1)[mine]
	if before["last_status"] != "ok" || before["repeat"].(map[string]any)["completed"] != float64(1) || len(turns.inputs) != 1 {
		t.Fatalf("the agent's job did not run: %v (turns %d)", before, len(turns.inputs))
	}
	if !strings.Contains(turns.inputs[0], "stale deals: ACME-1 ACME-2") {
		t.Errorf("the job's script did not feed its run: %s", turns.inputs[0])
	}

	// 3. Nap, dream: the agent's job is proposed for the repo.
	nap, err := loop.NapOnce(loop.NapOptions{Agent: "atlas", Unit: agent.Unit, Engine: eng, Home: home1, Store: store, Instance: "nt-1", BaselineDir: base, Now: clock})
	if err != nil || nap == nil {
		t.Fatalf("nap %v %v", nap, err)
	}
	napped := map[string]loop.NapFile{}
	for _, f := range nap.Files {
		napped[f.Path] = f
	}
	for _, p := range []string{"cron/jobs.json", "scripts/stale_sweep.sh", "scripts/lib/deals.sh"} {
		if napped[p].Class != "learning" {
			t.Errorf("%s napped as %q", p, napped[p].Class)
		}
	}
	dream, err := loop.Dream(inst, "atlas", store)
	if err != nil {
		t.Fatal(err)
	}
	list, meta, err := loop.ReadSchedulesProposal(root, "atlas")
	if err != nil || !dream.CronProposal || meta == nil || !slices.Contains(meta.RuntimeJobs, mine) {
		t.Fatalf("schedules proposal: %v %+v %v (dream %+v)", list, meta, err, dream)
	}
	// the job goes to review with the scripts it runs, so accepting it can name them
	proposed := map[string]loop.ScriptChange{}
	for _, c := range meta.Scripts {
		proposed[c.Name] = c
	}
	if c := proposed["stale_sweep.sh"]; c.Status != "new" || c.Sha256 != napped["scripts/stale_sweep.sh"].Sha256 || proposed["lib/deals.sh"].Status != "new" || len(proposed) != 2 {
		t.Errorf("proposed scripts = %+v", meta.Scripts)
	}
	if body, _ := os.ReadFile(filepath.Join(loop.SchedulesScriptsDir(root, "atlas"), "stale_sweep.sh")); string(body) != sweep {
		t.Errorf("proposed stale_sweep.sh = %q", body)
	}

	// 4. The next boot rehydrates from the nap: the agent's job keeps its definition and run
	// state, the repo's job its repo definition with its own run state.
	home2 := filepath.Join(tmp, "home2")
	if r := rehydrate(home2); !slices.Equal(r.RestoredScripts, []string{"lib/deals.sh", "stale_sweep.sh"}) {
		t.Errorf("restored scripts = %v", r.RestoredScripts)
	}
	for rel, want := range map[string]string{"stale_sweep.sh": sweep, "lib/deals.sh": deals} {
		if body, _ := os.ReadFile(filepath.Join(home2, "scripts", rel)); string(body) != want {
			t.Errorf("scripts/%s after the rehydrate = %q", rel, body)
		}
	}
	after := jobsIn(t, home2)
	j := after[mine]
	if j == nil {
		t.Fatalf("the agent's job did not survive the rehydrate: %v", after)
	}
	for _, k := range []string{"name", "prompt", "schedule", "skills", "enabled_toolsets", "deliver", "last_run_at", "last_status", "next_run_at", "repeat", "created_at"} {
		a, _ := json.Marshal(before[k])
		b, _ := json.Marshal(j[k])
		if string(a) != string(b) {
			t.Errorf("%s: %s before the rehydrate, %s after", k, a, b)
		}
	}
	if got := after[repoJob.ID]; got["prompt"] != repoJob.Prompt {
		t.Errorf("the repo job's definition did not win: %v", got["prompt"])
	}

	// 5. And it runs again on the new home when its time comes.
	clock = clock.Add(2*time.Hour + time.Minute)
	turns.reply = "No stale deals."
	s2 := sched.New(sched.Options{Home: home2, Turns: turns, Now: func() time.Time { return clock }})
	if err := s2.Boot(); err != nil {
		t.Fatal(err)
	}
	s2.Tick()
	s2.Wait()
	if j := jobsIn(t, home2)[mine]; j["repeat"].(map[string]any)["completed"] != float64(2) || len(turns.inputs) != 2 || !strings.Contains(turns.inputs[1], "Sweep deals with no activity") ||
		!strings.Contains(turns.inputs[1], "stale deals: ACME-1 ACME-2") {
		t.Errorf("after the rehydrate: %v (turns %d)", j, len(turns.inputs))
	}
	t.Logf("agent job %s: %s %s, next %v; proposal runtime jobs %v", mine, j["last_status"], j["schedule"], j["next_run_at"], meta.RuntimeJobs)

	// 6. A reviewer rejects the agent's schedules (review records the job and its scripts in the
	// watermark): the next build carries them, and the real rehydrate drops the job from the
	// napped file (Engine ScheduleDropper) and leaves its scripts out, while the repo's job stays.
	if _, err := loop.NapOnce(loop.NapOptions{Agent: "atlas", Unit: agent.Unit, Engine: eng, Home: home2, Store: store, Instance: "nt-2", BaselineDir: base, Now: clock}); err != nil {
		t.Fatal(err)
	}
	if _, err := loop.DecideSchedules(inst, "atlas", "rejected", loop.Decider{By: "test"}); err != nil {
		t.Fatal(err)
	}
	wm, err := loop.LoadWatermark(root, "atlas")
	if err != nil || !slices.Contains(wm.RejectedJobs, mine) || wm.RejectedScripts["stale_sweep.sh"] != napped["scripts/stale_sweep.sh"].Sha256 {
		t.Fatalf("watermark after the reject: %+v %v", wm, err)
	}
	_ = os.RemoveAll(base)
	if _, err := build.Agent(inst, "atlas", build.Options{Out: base, Target: manifest.AWS}); err != nil {
		t.Fatal(err)
	}
	home3 := filepath.Join(tmp, "home3")
	if r := rehydrate(home3); len(r.RestoredScripts) != 0 {
		t.Errorf("rejected scripts restored: %v", r.RestoredScripts)
	}
	for _, rel := range []string{"stale_sweep.sh", "lib/deals.sh"} {
		if _, err := os.Stat(filepath.Join(home3, "scripts", rel)); !os.IsNotExist(err) {
			t.Errorf("the rejected scripts/%s came back: %v", rel, err)
		}
	}
	final := jobsIn(t, home3)
	if _, ok := final[mine]; ok {
		t.Errorf("the rejected job came back: %v", final[mine])
	}
	if _, ok := final[repoJob.ID]; !ok {
		t.Errorf("the repo job was dropped too: %v", final)
	}
}

// serve runs a due job on its own: through the API's turn slots, in a cron session the core's
// hooks name with platform "cron", recorded on the job, and shown in the activity.
func TestServeRunsADueJob(t *testing.T) {
	shared := t.TempDir()
	_ = os.MkdirAll(filepath.Join(shared, "group"), 0o755)
	c, srv := newCoreStub(t, shared)
	h := home(t, srv.URL+"/v1", srv.URL+"/v1")
	cfg, _ := os.ReadFile(filepath.Join(h, "needletail.yaml"))
	_ = os.WriteFile(filepath.Join(h, "needletail.yaml"), bytes.ReplaceAll(cfg, []byte("http://127.0.0.1:1"), []byte(srv.URL)), 0o644)
	jobsPath := filepath.Join(h, "cron", "jobs.json")
	f, err := sched.ReadFile(jobsPath)
	if err != nil {
		t.Fatal(err)
	}
	j := sched.NewJob()
	for k, v := range map[string]any{"id": "due0001", "name": "Nova ping", "prompt": "Tell nova the digest is ready.", "skills": []string{"crm-api"},
		"no_agent": false, "schedule": map[string]any{"kind": "interval", "minutes": 60, "display": "every 60m"}, "enabled": true,
		"state": "scheduled", "next_run_at": "2026-01-01T00:00:00Z", "deliver": "local", "repeat": map[string]any{"times": nil, "completed": 0}} {
		j.Set(k, v)
	}
	f.Jobs = append(f.Jobs, j)
	if err := f.Write(jobsPath, filepath.Join(h, ".needletail-tmp")); err != nil {
		t.Fatal(err)
	}
	setenv(t, h)
	t.Setenv("SWARM_SHARED_DIR", shared)
	port := freePort(t)
	t.Setenv("NEEDLETAIL_API_PORT", strconv.Itoa(port))
	var stderr bytes.Buffer
	var mu sync.Mutex
	logs := &lockedWriter{w: &stderr, mu: &mu}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int)
	go func() { done <- Run(ctx, []string{"serve"}, io.Discard, logs) }()
	var job map[string]any
	for i := 0; i < 500; i++ {
		job = jobsIn(t, h)["due0001"]
		if job["last_status"] != nil && job["running"] == false {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job["last_status"] != "ok" || job["running"] != false {
		mu.Lock()
		t.Fatalf("job = %v\n%s", job, stderr.String())
	}
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/v1/activity", port), nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var act wire.Activity
	_ = json.NewDecoder(res.Body).Decode(&act)
	res.Body.Close()
	if act.LastJobAt == "" || act.Source != "cron" {
		t.Errorf("activity = %+v", act)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
	sessions, _ := filepath.Glob(filepath.Join(h, "sessions", "cron-due0001-*.jsonl"))
	if len(sessions) != 1 {
		t.Fatalf("cron sessions %v", sessions)
	}
	head, _ := os.ReadFile(sessions[0])
	if !strings.Contains(strings.SplitN(string(head), "\n", 2)[0], `"source":"cron"`) || !strings.Contains(string(head), `"end_reason":"cron_complete"`) {
		t.Errorf("session header %s", strings.SplitN(string(head), "\n", 2)[0])
	}
	var platforms []string
	for i := 0; i < 100; i++ {
		rows, err := c.db.Query(`SELECT preview FROM activity WHERE event = 'session_start'`)
		if err != nil {
			t.Fatal(err)
		}
		platforms = nil
		for rows.Next() {
			var p string
			_ = rows.Scan(&p)
			platforms = append(platforms, p)
		}
		rows.Close()
		if len(platforms) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !slices.Contains(platforms, "cron") {
		t.Errorf("the core saw sessions from %v", platforms)
	}
	if recs, _ := os.ReadDir(filepath.Join(h, "state", "needletail", "jobs", "due0001")); len(recs) != 1 {
		t.Errorf("run records %v", recs)
	}
}
