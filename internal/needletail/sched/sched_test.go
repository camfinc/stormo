package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/build"
	adapter "github.com/camfinc/stormo/pkg/engine/needletail"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/review"
)

// fixtureScript prints fixture/<name>.out from the home and exits with fixture/<name>.code.
const fixtureScript = `name=$(basename "$0"); cat "$HOME/fixture/$name.out" 2>/dev/null; [ -f "$HOME/fixture/$name.pwd" ] && pwd; exit $(cat "$HOME/fixture/$name.code" 2>/dev/null || echo 0)` + "\n"

const fixturePy = `import os, sys
name = os.path.basename(sys.argv[0])
base = os.path.join(os.environ["HOME"], "fixture", name)
try:
    print(open(base + ".out").read(), end="")
except FileNotFoundError:
    pass
try:
    sys.exit(int(open(base + ".code").read()))
except FileNotFoundError:
    sys.exit(0)
`

// buildHome is `stormo build` of the example atlas agent on needletail with testdata's
// schedules (anonymised real job definitions) and fixture scripts: the home the runtime boots in.
func buildHome(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "instance")
	if err := os.CopyFS(root, os.DirFS(filepath.Join("..", "..", "..", "examples", "minimal"))); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(root, "agents", "atlas")
	raw, err := os.ReadFile(filepath.Join(agentDir, "agent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "schedules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := string(raw)
	start, end := strings.Index(m, "\nschedules:\n"), strings.Index(m, "\nstate:\n")
	if start < 0 || end < start {
		t.Fatal("agent.yaml has no schedules block before state")
	}
	m = m[:start+1] + string(fixture) + m[end:]
	if err := os.WriteFile(filepath.Join(agentDir, "agent.yaml"), []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(agentDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"tracker_watch.sh", "sla_watchdog.sh", "perf_monitor.sh", "radar_fetch.sh"} {
		if err := os.WriteFile(filepath.Join(agentDir, "scripts", s), []byte(fixtureScript), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scripts", "collect.py"), []byte(fixturePy), 0o755); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(tmp, "home")
	if _, err := build.Agent(inst, "atlas", build.Options{Out: home, Target: manifest.AWS}); err != nil {
		t.Fatal(err)
	}
	return home
}

// fakeTurns hands out slots whose turns answer with the next scripted reply for the job.
type fakeTurns struct {
	mu      sync.Mutex
	busy    bool
	replies map[string][]string // job id → replies ("!err" fails the turn)
	calls   []turnCall
	// onTurn runs at the start of each turn, outside the lock (a change made while a job runs).
	onTurn func()
	// block makes turns wait for their context to end (a turn the shutdown cancels).
	block bool
	// reserved counts Reserve calls that got a slot; held is the slots not yet released.
	reserved, held int
	// lane, when set, is how many slots may be held at once (the jobs' lane; more is busy).
	lane int
}

type turnCall struct {
	job, session, input string
	tools               []string
}

func jobOf(session string) string {
	parts := strings.Split(session, "-")
	return parts[1]
}

func (f *fakeTurns) Reserve(session string, create wire.Session) (Slot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy || f.lane > 0 && f.held >= f.lane {
		return nil, errors.New("the agent is running as many turns as it can")
	}
	if create.Source != SourceCron || create.Title == "" {
		return nil, fmt.Errorf("session created as %+v", create)
	}
	f.reserved++
	f.held++
	return &fakeSlot{f: f, session: session}, nil
}

func (f *fakeTurns) reply(job string, text ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.replies == nil {
		f.replies = map[string][]string{}
	}
	f.replies[job] = append(f.replies[job], text...)
}

func (f *fakeTurns) taken() []turnCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

type fakeSlot struct {
	f        *fakeTurns
	session  string
	released bool
}

func (s *fakeSlot) Turn(ctx context.Context, input string, opts loop.Options) (*loop.Result, error) {
	f := s.f
	f.mu.Lock()
	hook, block := f.onTurn, f.block
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if block {
		<-ctx.Done()
		return nil, fmt.Errorf("model endpoint 0: %w", ctx.Err())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	job := jobOf(s.session)
	f.calls = append(f.calls, turnCall{job: job, session: s.session, input: input, tools: opts.Tools})
	reply := "(no reply scripted)"
	if q := f.replies[job]; len(q) > 0 {
		reply, f.replies[job] = q[0], q[1:]
	}
	if reply == "!err" {
		return nil, errors.New("model endpoint 0: HTTP 502")
	}
	return &loop.Result{Text: reply, Session: s.session}, nil
}

func (s *fakeSlot) Release() {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	if !s.released {
		s.f.held--
	}
	s.released = true
}

// sink records deliveries.
type sink struct {
	mu  sync.Mutex
	got []Delivery
}

func (k *sink) Deliver(_ context.Context, d Delivery) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if d.Target.Platform == "telegram" {
		return errors.New("needletail does not deliver to telegram")
	}
	k.got = append(k.got, d)
	return nil
}

func (k *sink) taken() []Delivery {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := k.got
	k.got = nil
	return out
}

type rig struct {
	t     *testing.T
	home  string
	now   time.Time
	s     *Scheduler
	turns *fakeTurns
	sink  *sink
	ended []string
	logs  []string
	mu    sync.Mutex
}

var allTools = []string{"terminal", "read_file", "write_file", "patch", "search_files", "memory", "skills_list", "skill_view",
	"skill_manage", CronJobTool, "send_message", "no_reply", "mcp_core_msg_send", "mcp_core_fs_note"}

func newRig(t *testing.T, home string, now time.Time) *rig {
	r := &rig{t: t, home: home, now: now, turns: &fakeTurns{}, sink: &sink{}}
	r.s = New(Options{Home: home, Turns: r.turns, Skills: &skills.Library{Home: home, Generated: []string{"acme-knowledge", "acme-shared-docs"}},
		Tools: func() []string { return allTools }, Env: tools.Env{Home: home, Redact: func(s string) string { return strings.ReplaceAll(s, "sekret-value", "[REDACTED]") }},
		Deliver: r.sink, End: func(s, reason string) error {
			r.mu.Lock()
			r.ended = append(r.ended, s+":"+reason)
			r.mu.Unlock()
			return nil
		},
		Now: func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }, Log: func(l string) { r.mu.Lock(); r.logs = append(r.logs, l); r.mu.Unlock() },
		ScriptTimeout: 10 * time.Second})
	return r
}

func (r *rig) advance(d time.Duration) {
	r.mu.Lock()
	r.now = r.now.Add(d)
	r.mu.Unlock()
}

func (r *rig) tick() {
	r.t.Helper()
	r.s.Tick()
	r.s.Wait()
}

func (r *rig) fixture(script, out string, code int) {
	r.t.Helper()
	dir := filepath.Join(r.home, "fixture")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, script+".out"), []byte(out), 0o644); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, script+".code"), []byte(fmt.Sprint(code)), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) job(id string) *Job {
	r.t.Helper()
	f, err := ReadFile(filepath.Join(r.home, "cron", "jobs.json"))
	if err != nil {
		r.t.Fatal(err)
	}
	j := f.Find(id)
	if j == nil {
		r.t.Fatalf("no job %s", id)
	}
	return j
}

// due makes one job due now (the others keep their times).
func (r *rig) due(id string) {
	r.t.Helper()
	if err := r.s.mutate(func(f *File) (bool, error) {
		f.Find(id).Set("next_run_at", r.s.at(r.s.o.Now()))
		return true, nil
	}); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) state(id string) string {
	j := r.job(id)
	return fmt.Sprintf("%s streak=%d err=%q", j.Str("last_status"), j.Int("failure_streak"), j.Str("last_error"))
}

const (
	watch    = "1a2b3c4d5e01"
	weekly   = "1a2b3c4d5e02"
	checkin  = "1a2b3c4d5e03"
	sla      = "1a2b3c4d5e04"
	collect  = "1a2b3c4d5e05"
	perf     = "1a2b3c4d5e06"
	radar    = "1a2b3c4d5e07"
	briefs   = "1a2b3c4d5e08"
	t0String = "2026-10-21T09:00:00Z" // a Wednesday
)

// M4's done-when, first part (docs/needletail.md §9): table tests built from real agent job
// definitions. The jobs are rendered by the real build; the runtime boots on them, runs them as
// their schedules say, and records, gates and delivers as Hermes does (docs/schedules.md).
func TestRealJobs(t *testing.T) {
	home := buildHome(t)
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := newRig(t, home, t0)
	if err := r.s.Boot(); err != nil {
		t.Fatal(err)
	}

	// Boot gives every enabled job its next run on its own schedule; the paused one keeps none.
	for id, want := range map[string]string{
		watch: "2026-10-21T11:00:00Z", weekly: "2026-10-26T10:00:00Z", sla: "2026-10-21T09:02:00Z", collect: "2026-10-21T15:00:00Z",
		perf: "2026-10-21T10:00:00Z", radar: "2026-10-22T08:00:00Z", briefs: "2026-10-21T10:00:00Z", checkin: "",
	} {
		if got := r.job(id).Str("next_run_at"); got != want {
			t.Errorf("job %s next run %q, want %q", id, got, want)
		}
	}
	if j := r.job(checkin); j.Enabled() || j.Str("paused_reason") != "paused pending review" {
		t.Errorf("paused job: enabled %v reason %q", j.Enabled(), j.Str("paused_reason"))
	}
	r.tick()
	if c := r.turns.taken(); len(c) != 0 {
		t.Fatalf("nothing is due yet, ran %+v", c)
	}

	type step struct {
		name    string
		at      time.Duration // since the previous step
		prepare func()
		job     string
		status  string // the job's state after the tick
		turn    []string
		noTurn  bool
		deliver string // "target|text fragment", "" none
		check   func(t *testing.T, c []turnCall)
	}
	steps := []step{
		{name: "script-only watchdog: its output is delivered as is", at: 2 * time.Minute, job: sla,
			prepare: func() { r.fixture("sla_watchdog.sh", "SLA: 2 chats over 60 min (sekret-value)\n", 0) },
			status:  "ok streak=0 err=\"\"", noTurn: true, deliver: "slack (home)|SLA: 2 chats over 60 min ([REDACTED])"},
		{name: "a script runs in its own directory, as in Hermes", at: 2 * time.Minute, job: sla,
			prepare: func() {
				r.fixture("sla_watchdog.sh", "cwd: ", 0)
				_ = os.WriteFile(filepath.Join(home, "fixture", "sla_watchdog.sh.pwd"), nil, 0o644)
			},
			status: "ok streak=0 err=\"\"", noTurn: true, deliver: "slack (home)|cwd: " + realScripts(t, home)},
		{name: "script-only watchdog: empty output is silent", at: 2 * time.Minute, job: sla,
			prepare: func() {
				_ = os.Remove(filepath.Join(home, "fixture", "sla_watchdog.sh.pwd"))
				r.fixture("sla_watchdog.sh", "", 0)
			},
			status: "silent streak=0 err=\"\"", noTurn: true},
		{name: "script-only watchdog: the wake gate is silent", at: 2 * time.Minute, job: sla,
			prepare: func() { r.fixture("sla_watchdog.sh", "checked 14 chats\n{\"wakeAgent\": false}\n", 0) }, status: "silent streak=0 err=\"\"", noTurn: true},
		{name: "script-only watchdog: a failing script alerts the failure lane", at: 2 * time.Minute, job: sla,
			prepare: func() { r.fixture("sla_watchdog.sh", "partial", 3) }, noTurn: true,
			status: "error streak=1 err=\"Script exited with code 3\\nstdout:\\npartial\"", deliver: "slack (home)|script failed"},
		{name: "collector (python) delivers to its channel", at: 0, job: collect,
			prepare: func() { r.due(collect); r.fixture("collect.py", "collected 3 accounts", 0) }, noTurn: true,
			status: "ok streak=0 err=\"\"", deliver: "slack:C0000000002|collected 3 accounts"},
		{name: "monitor, first run: the baseline wakes the agent with its skill loaded", at: 2 * time.Hour, job: watch,
			prepare: func() {
				r.fixture("tracker_watch.sh", "FINDING alpha owner=a\nFINDING beta owner=b", 0)
				r.turns.reply(watch, "Two findings: alpha (a), beta (b).")
			},
			status: "ok streak=0 err=\"\"", turn: []string{"Monitor Baseline (first run)", "FINDING beta owner=b", "[Scheduled job:", `The job loads the "crm-api" skill`, "Query Acme deals", "If findings only disappeared"},
			deliver: "slack (home)|Two findings",
			check: func(t *testing.T, c []turnCall) {
				if slices.Contains(c[0].tools, CronJobTool) || slices.Contains(c[0].tools, "send_message") || slices.Contains(c[0].tools, "no_reply") || !slices.Contains(c[0].tools, "terminal") || !slices.Contains(c[0].tools, "mcp_core_msg_send") {
					t.Errorf("a job with no toolsets got %v", c[0].tools)
				}
				if !strings.HasPrefix(c[0].session, "cron-"+watch+"-") {
					t.Errorf("session %s", c[0].session)
				}
			}},
		{name: "monitor unchanged: no run, no delivery", at: 2 * time.Hour, job: watch,
			prepare: func() { r.fixture("tracker_watch.sh", "FINDING alpha owner=a\nFINDING beta owner=b", 0) },
			status:  "no_change streak=0 err=\"\"", noTurn: true},
		{name: "monitor changed: the diff wakes the agent, [SILENT] is not delivered", at: 2 * time.Hour, job: watch,
			prepare: func() {
				r.fixture("tracker_watch.sh", "FINDING alpha owner=a", 0)
				r.turns.reply(watch, "[SILENT]")
			},
			status: "silent streak=0 err=\"\"", turn: []string{"MONITOR CHANGE DETECTED", "-FINDING beta owner=b", "### Current output"}},
		{name: "monitor fails: an error, the hash kept", at: 2 * time.Hour, job: watch,
			prepare: func() { r.fixture("tracker_watch.sh", "", 1) }, noTurn: true,
			status: "error streak=1 err=\"monitor: Script exited with code 1\"", deliver: "slack (home)|monitor failed"},
		{name: "monitor back with the same output: still no change", at: 2 * time.Hour, job: watch,
			prepare: func() { r.fixture("tracker_watch.sh", "FINDING alpha owner=a", 0) }, status: "no_change streak=0 err=\"\"", noTurn: true},
		{name: "a pre-run script's output goes into the prompt", at: 0, job: radar,
			prepare: func() {
				r.due(radar)
				r.fixture("radar_fetch.sh", "3 new items", 0)
				r.turns.reply(radar, "Good morning: three new items.")
			},
			status: "ok streak=0 err=\"\"", turn: []string{"## Script Output", "3 new items", "Run the news radar"}, deliver: "slack (home)|three new items"},
		{name: "a pre-run script that prints nothing skips the turn", at: 0, job: radar,
			prepare: func() { r.due(radar); r.fixture("radar_fetch.sh", "", 0) }, status: "silent streak=0 err=\"\"", noTurn: true},
		{name: "a failing pre-run script is reported to the agent, not a failure", at: 0, job: radar,
			prepare: func() {
				r.due(radar)
				r.fixture("radar_fetch.sh", "feed down", 2)
				r.turns.reply(radar, "The feed is down today.")
			},
			status: "ok streak=0 err=\"\"", turn: []string{"## Script Error", "Script exited with code 2"}, deliver: "slack (home)|feed is down"},
		{name: "toolsets expand, a generated skill loads, a failure with on_failure local is not delivered", at: 0, job: briefs,
			prepare: func() { r.due(briefs); r.turns.reply(briefs, "!err") },
			status:  "error streak=1 err=\"model endpoint 0: HTTP 502\"", turn: []string{`The job loads the "acme-shared-docs" skill`, `"news-radar" skill`},
			check: func(t *testing.T, c []turnCall) {
				want := []string{"terminal", "skills_list", "skill_view", "skill_manage", "mcp_core_msg_send", "mcp_core_fs_note"}
				if !sameSet(c[0].tools, want) {
					t.Errorf("tools %v, want %v", c[0].tools, want)
				}
			}},
		{name: "failures in a row add the review nudge", at: 0, job: perf,
			prepare: func() {
				r.fixture("perf_monitor.sh", "SIGNAL reach down", 0)
				for range 3 {
					r.due(perf)
					r.turns.reply(perf, "[CRON_FAILURE]\nthe metrics store is unreadable")
					r.fixture("perf_monitor.sh", fmt.Sprintf("SIGNAL reach down %d", time.Now().UnixNano()), 0)
					if r.s.Tick(); true {
						r.s.Wait()
					}
				}
				r.sink.taken()
				r.turns.taken()
				r.due(perf)
				r.fixture("perf_monitor.sh", "SIGNAL final", 0)
				r.turns.reply(perf, "!err")
			},
			status: "error streak=4 err=\"model endpoint 0: HTTP 502\"", turn: []string{"SIGNAL final"},
			deliver: "slack:C0000000002|failed 4 runs in a row: worth a review"},
		{name: "a success resets the streak", at: 0, job: perf,
			prepare: func() {
				r.due(perf)
				r.fixture("perf_monitor.sh", "SIGNAL ok", 0)
				r.turns.reply(perf, "Reach recovered.")
			},
			status: "ok streak=0 err=\"\"", turn: []string{"SIGNAL ok"}, deliver: "slack:C0000000002|Reach recovered"},
	}
	for _, st := range steps {
		r.advance(st.at)
		if st.prepare != nil {
			st.prepare()
		}
		r.tick()
		calls := []turnCall{}
		for _, c := range r.turns.taken() {
			if c.job == st.job {
				calls = append(calls, c)
			}
		}
		if got := r.state(st.job); got != st.status {
			t.Errorf("%s: state %s, want %s", st.name, got, st.status)
		}
		if st.noTurn && len(calls) != 0 {
			t.Errorf("%s: ran a turn: %+v", st.name, calls)
		}
		if len(st.turn) > 0 {
			if len(calls) != 1 {
				t.Errorf("%s: %d turns", st.name, len(calls))
				continue
			}
			for _, w := range st.turn {
				if !strings.Contains(calls[0].input, w) {
					t.Errorf("%s: the prompt lacks %q:\n%s", st.name, w, calls[0].input)
				}
			}
		}
		if st.check != nil && len(calls) > 0 {
			st.check(t, calls)
		}
		var mine []Delivery
		for _, d := range r.sink.taken() {
			if d.JobID == st.job {
				mine = append(mine, d)
			}
		}
		switch {
		case st.deliver == "" && len(mine) > 0:
			t.Errorf("%s: delivered %+v", st.name, mine)
		case st.deliver != "":
			target, text, _ := strings.Cut(st.deliver, "|")
			if len(mine) != 1 || mine[0].Target.String() != target || !strings.Contains(mine[0].Text, text) {
				t.Errorf("%s: delivered %+v, want %s", st.name, mine, st.deliver)
			}
		}
	}

	// Each run's session was ended; runs are recorded (no_change leaves no record), newest kept.
	r.mu.Lock()
	ended := strings.Join(r.ended, " ")
	r.mu.Unlock()
	if !strings.Contains(ended, "cron-"+watch+"-") || !strings.Contains(ended, ":cron_complete") {
		t.Errorf("ended sessions %s", ended)
	}
	recs, _ := os.ReadDir(filepath.Join(home, "state", "needletail", "jobs", sla))
	_, slaRuns := r.job(sla).Repeat() // every 2m: it ran at each later step too
	if len(recs) != min(slaRuns, keepOutputs) || slaRuns < 4 {
		t.Errorf("sla: %d records of %d runs", len(recs), slaRuns)
	}
	if body, _ := os.ReadFile(filepath.Join(home, "state", "needletail", "jobs", sla, recs[0].Name())); !strings.Contains(string(body), "[REDACTED]") || strings.Contains(string(body), "sekret-value") {
		t.Errorf("first record:\n%s", body)
	}
	// The monitor's snapshot and hash live where docs say; next runs are re-anchored on the run.
	if snap, _ := os.ReadFile(filepath.Join(home, "state", "needletail", "monitor", watch+".out")); string(snap) != "FINDING alpha owner=a" {
		t.Errorf("snapshot %q", snap)
	}
	if ms, ok := r.job(watch).monitor(); !ok || len(ms.LastOutputHash) != 64 {
		t.Errorf("monitor_state %+v", ms)
	}
	if j := r.job(watch); j.Str("next_run_at") != r.s.at(r.now.Add(2*time.Hour)) || j.Bool("running") {
		t.Errorf("watch next %s running %v", j.Str("next_run_at"), j.Bool("running"))
	}
	for _, tmp := range []string{"cron", filepath.Join("state", "needletail", "monitor"), filepath.Join("state", "needletail", "jobs", sla)} {
		entries, _ := os.ReadDir(filepath.Join(home, tmp))
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp-") {
				t.Errorf("temp file in %s: %s", tmp, e.Name())
			}
		}
	}
}

// realScripts is the home's scripts/ with symlinks resolved (macOS' /var is /private/var).
func realScripts(t *testing.T, home string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(filepath.Join(home, "scripts"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// A busy agent leaves a due job due (it runs on the next tick); a run cut short by a restart is
// recorded as failed at boot; a job whose payload is empty is paused, never retried every tick.
func TestBusyRestartAndUnrunnable(t *testing.T) {
	home := buildHome(t)
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := newRig(t, home, t0)
	if err := r.s.Boot(); err != nil {
		t.Fatal(err)
	}
	r.due(weekly)
	r.turns.busy = true
	r.tick()
	if j := r.job(weekly); j.Str("last_run_at") != "" || j.Str("next_run_at") != t0String {
		t.Errorf("busy: last %q next %q", j.Str("last_run_at"), j.Str("next_run_at"))
	}
	r.turns.busy = false
	r.turns.reply(weekly, "Status: on track.")
	r.advance(30 * time.Second)
	r.tick()
	if got := r.state(weekly); got != `ok streak=0 err=""` || r.job(weekly).Str("next_run_at") != "2026-10-26T10:00:00Z" {
		t.Errorf("after busy: %s next %s", got, r.job(weekly).Str("next_run_at"))
	}

	_ = r.s.mutate(func(f *File) (bool, error) {
		j := f.Find(weekly)
		j.Set("running", true)
		j.Set("prompt", "")
		j.Set("skills", []string{})
		j.Set("next_run_at", r.s.at(r.now))
		return true, nil
	})
	again := newRig(t, home, r.now)
	if err := again.s.Boot(); err != nil {
		t.Fatal(err)
	}
	if got := again.state(weekly); got != `error streak=1 err="interrupted: the runtime stopped before the run finished"` || again.job(weekly).Bool("running") {
		t.Errorf("after restart: %s", got)
	}
	again.tick()
	j := again.job(weekly)
	if j.Enabled() || j.Str("state") != "paused" || !strings.Contains(j.Str("paused_reason"), "Auto-paused by the scheduler: the job has no prompt") || j.Int("failure_streak") != 2 {
		t.Errorf("unrunnable job: enabled %v state %s reason %q streak %d", j.Enabled(), j.Str("state"), j.Str("paused_reason"), j.Int("failure_streak"))
	}
	if d := again.sink.taken(); len(d) != 1 || !strings.Contains(d[0].Text, "was paused") {
		t.Errorf("pause notice %+v", d)
	}
	again.tick()
	if c := again.turns.taken(); len(c) != 0 {
		t.Errorf("a paused job ran: %+v", c)
	}
}

// The cronjob_manage tool: Hermes' actions on the agent's own jobs, through the scheduler's
// single writer; a job it makes runs like any other.
func TestCronJobTool(t *testing.T) {
	home := buildHome(t)
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := newRig(t, home, t0)
	if err := r.s.Boot(); err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry(r.s.Tool(func(session string) json.RawMessage {
		if session == "slack-C0000000009-1700000000.000100" {
			return json.RawMessage(`{"platform":"slack","chat_id":"C0000000009","thread_id":"1700000000.000100"}`)
		}
		return nil
	}))
	call := func(session string, args map[string]any) map[string]any {
		t.Helper()
		b, _ := json.Marshal(args)
		out, failed := reg.Call(context.Background(), &tools.Env{Session: session}, CronJobTool, string(b))
		var v map[string]any
		_ = json.Unmarshal([]byte(out), &v)
		if failed {
			v["failed"] = true
		}
		return v
	}
	thread := "slack-C0000000009-1700000000.000100"
	made := call(thread, map[string]any{"action": "create", "schedule": "every 30m", "prompt": "Check the deal pipeline and report changes.",
		"skills": []string{"crm-api"}, "repeat": 2, "enabled_toolsets": []string{"terminal", "skills"}})
	job, _ := made["job"].(map[string]any)
	id, _ := job["job_id"].(string)
	if len(id) != 12 || job["next_run_at"] != "2026-10-21T09:30:00Z" || job["schedule"] != "every 30m" || job["deliver"] != "origin" {
		t.Fatalf("create = %v", made)
	}
	for _, bad := range []map[string]any{
		{"action": "create", "schedule": "in 30m", "prompt": "x"},
		{"action": "create", "schedule": "every 30m"},
		{"action": "create", "schedule": "every 30m", "prompt": "x", "script": "../../etc/passwd"},
		{"action": "create", "schedule": "every 30m", "no_agent": true},
		{"action": "create", "schedule": "every 30m", "no_agent": true, "script": "sla_watchdog.sh", "monitor": "perf_monitor.sh"},
		{"action": "create", "schedule": "every 30m", "prompt": "x", "skills": []string{"no-such-skill"}},
		{"action": "pause", "job_id": "nope"},
		{"action": "update"},
		{"action": "explode", "job_id": id},
	} {
		if v := call("api-1", bad); v["failed"] != true {
			t.Errorf("%v accepted: %v", bad, v)
		}
	}
	list := call("api-1", map[string]any{"action": "list"})
	if n, _ := list["count"].(float64); n != 9 {
		t.Errorf("list count %v", list["count"])
	}

	// It runs: delivered to the thread it was made in, with its toolsets, twice, then completes.
	r.advance(30 * time.Minute)
	r.turns.reply(id, "Pipeline: two deals moved.", "Pipeline: quiet.")
	r.tick()
	d := r.sink.taken()
	c := r.turns.taken()
	if len(d) != 1 || d[0].Target.String() != "slack:C0000000009:1700000000.000100" || len(c) != 1 || slices.Contains(c[0].tools, CronJobTool) {
		t.Errorf("first run: deliveries %+v calls %+v", d, c)
	}
	call("api-1", map[string]any{"action": "run", "job_id": id})
	r.tick()
	j := r.job(id)
	if j.Enabled() || j.Str("state") != "completed" || j.Str("next_run_at") != "" {
		t.Errorf("after two runs: enabled %v state %s next %q", j.Enabled(), j.Str("state"), j.Str("next_run_at"))
	}
	if v := call("api-1", map[string]any{"action": "run", "job_id": id}); v["failed"] != true {
		t.Errorf("a completed job ran: %v", v)
	}

	// Pause, resume, update and remove a repo job and an agent job.
	call("api-1", map[string]any{"action": "pause", "job_id": weekly, "reason": "holiday week"})
	if j := r.job(weekly); j.Enabled() || j.Str("paused_reason") != "holiday week" || j.Str("paused_at") == "" {
		t.Errorf("paused: %v %q", j.Enabled(), j.Str("paused_reason"))
	}
	r.advance(10 * 24 * time.Hour) // its Monday run passes while it is paused
	call("api-1", map[string]any{"action": "resume", "job_id": weekly})
	if j := r.job(weekly); !j.Enabled() || j.Str("state") != "scheduled" || j.Str("next_run_at") != "2026-10-26T10:00:00Z" {
		t.Errorf("resumed: %v %s next %s", j.Enabled(), j.Str("state"), j.Str("next_run_at"))
	}
	upd := call("api-1", map[string]any{"action": "update", "job_id": weekly, "schedule": "weekdays at 9am", "deliver": "slack:C0000000003", "failure_deliver": ""})
	if j := r.job(weekly); j.Schedule().Expr != "0 9 * * 1-5" || j.Str("deliver") != "slack:C0000000003" || j.Str("failure_deliver") != "" || j.Str("next_run_at") != "2026-11-02T09:00:00Z" {
		t.Errorf("update = %v; job %s %s %s", upd, j.Schedule().Expr, j.Str("deliver"), j.Str("next_run_at"))
	}
	if v := call("api-1", map[string]any{"action": "remove", "job_id": id}); v["removed"] != id {
		t.Errorf("remove = %v", v)
	}
	if f, _ := ReadFile(filepath.Join(home, "cron", "jobs.json")); f.Find(id) != nil || len(f.Jobs) != 8 {
		t.Errorf("after remove: %d jobs", len(f.Jobs))
	}
}

// `stormo review` reads the file the runtime writes through the adapter (Engine.Schedules) and
// flags what it should: a failed last run, a failure streak, a delivery error, an overdue job;
// an agent-made job reads back as a schedule (the dream's proposal) too.
func TestReviewReadsTheRuntimesFile(t *testing.T) {
	home := buildHome(t)
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := newRig(t, home, t0)
	if err := r.s.Boot(); err != nil {
		t.Fatal(err)
	}
	r.due(sla)
	r.fixture("sla_watchdog.sh", "boom", 1)
	r.tick()
	r.due(collect)
	_ = r.s.mutate(func(f *File) (bool, error) { f.Find(collect).Set("deliver", "telegram"); return true, nil })
	r.fixture("collect.py", "collected", 0)
	r.tick()
	made, err := r.s.manage(cronArgs{Action: "create", Schedule: ptr("every 45m"), Prompt: ptr("Ping the pipeline.")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(home, "cron", "jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	eng := adapter.New(&instance.Instance{})
	status, err := eng.Schedules(body)
	if err != nil {
		t.Fatal(err)
	}
	issues := review.AnalyzeSchedules(status, t0.Add(48*time.Hour))
	got := []string{}
	for _, i := range issues {
		got = append(got, i.Job+": "+i.Problem)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"SLA watchdog: last run error: Script exited with code 1", "SLA watchdog: failure streak 1",
		"Metrics collector: last run delivery_failed", "Metrics collector: delivery error: telegram (home): needletail does not deliver to telegram",
		"Tracker watch: overdue since 2026-10-21T11:00:00Z"} {
		if !strings.Contains(joined, want) {
			t.Errorf("review lacks %q:\n%s", want, joined)
		}
	}
	read, err := eng.ReadSchedules(body)
	if err != nil {
		t.Fatal(err)
	}
	id := made.(map[string]any)["job"].(jobView).JobID
	var mine *manifest.Schedule
	for i := range read {
		if read[i].ID == id {
			mine = &read[i]
		}
	}
	if mine == nil || mine.Every != "45m" || mine.Prompt != "Ping the pipeline." || mine.Deliver != "origin" || !mine.Agent || !mine.Enabled {
		t.Errorf("the agent's job reads back as %+v", mine)
	}
}

func ptr[T any](v T) *T { return &v }

// memory_manage reaches a scheduled run only when its list names it; memory_search comes with the
// memory toolset and the default set, as memory does.
func TestMemoryToolsets(t *testing.T) {
	all := []string{"cronjob_manage", "mcp_core_fs_ls", "memory", "memory_manage", "memory_search", "terminal"}
	for _, c := range []struct {
		enabled []string
		want    []string
	}{
		{nil, []string{"mcp_core_fs_ls", "memory", "memory_search", "terminal"}},
		{[]string{"all"}, []string{"mcp_core_fs_ls", "memory", "memory_search", "terminal"}},
		{[]string{"memory"}, []string{"mcp_core_fs_ls", "memory", "memory_search"}},
		{[]string{"memory", "memory_manage", "no_mcp"}, []string{"memory", "memory_manage", "memory_search"}},
	} {
		if got := ExpandToolsets(c.enabled, all); !slices.Equal(got, c.want) {
			t.Errorf("%v: %v, want %v", c.enabled, got, c.want)
		}
	}
}

// A scheduled run is never offered follow_up: it has no conversation to come back to, even when
// its list names the tool.
func TestNoFollowUpsInScheduledRuns(t *testing.T) {
	all := []string{"follow_up", "memory", "terminal"}
	for _, enabled := range [][]string{nil, {"all"}, {"follow_up", "terminal"}} {
		if got := ExpandToolsets(enabled, all); slices.Contains(got, FollowUpTool) {
			t.Errorf("%v: %v", enabled, got)
		}
	}
	if !strings.Contains(ErrOneShot.Error(), "follow_up") {
		t.Errorf("the one-time refusal does not point at follow_up: %v", ErrOneShot)
	}
}
