package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/tools"
)

// bareRig is a scheduler on an empty home holding only the given jobs (raw JSON objects).
func bareRig(t *testing.T, now time.Time, jobs ...string) *rig {
	t.Helper()
	home := t.TempDir()
	for _, d := range []string{"cron", "scripts", ".needletail-tmp"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"jobs": [` + strings.Join(jobs, ",") + `]}`
	if err := os.WriteFile(filepath.Join(home, "cron", "jobs.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return newRig(t, home, now)
}

func jobJSON(id, schedule, next string) string {
	return `{"id":"` + id + `","name":"` + id + `","prompt":"Report.","schedule":` + schedule +
		`,"enabled":true,"state":"scheduled","next_run_at":` + next + `,"deliver":"local","repeat":{"times":null,"completed":0}}`
}

// A job whose next run cannot be computed (a schedule needletail does not run, an interval
// too long to add) never runs on every tick: it gets no next run, state "error" and why, and
// the file is not rewritten tick after tick (Hermes: next_run_at None, state error).
func TestUncomputableNextRun(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	past := `"2026-10-21T08:00:00Z"`
	bad := map[string]string{
		"hourly":   `{"kind":"cron","expr":"@hourly","display":"@hourly"}`,
		"nth":      `{"kind":"cron","expr":"0 9 * * mon#1","display":"0 9 * * mon#1"}`,
		"last":     `{"kind":"cron","expr":"0 9 L * *","display":"0 9 L * *"}`,
		"huge":     `{"kind":"interval","minutes":288000000,"display":"every 288000000m"}`,
		"once":     `{"kind":"once","run_at":"2026-10-21T08:00:00Z","display":"once"}`,
		"feb30":    `{"kind":"cron","expr":"0 9 30 2 *","display":"0 9 30 2 *"}`,
		"negative": `{"kind":"interval","minutes":-5,"display":"every -5m"}`,
	}
	jobs := []string{}
	for id, s := range bad {
		jobs = append(jobs, jobJSON(id, s, past))
	}
	jobs = append(jobs, jobJSON("good", `{"kind":"interval","minutes":60,"display":"every 60m"}`, past))
	r := bareRig(t, t0, jobs...)
	r.turns.reply("good", "fine")
	var before []byte
	for i := 0; i < 3; i++ {
		if i == 2 {
			before, _ = os.ReadFile(filepath.Join(r.home, "cron", "jobs.json"))
		}
		r.tick()
		r.advance(30 * time.Second)
	}
	after, _ := os.ReadFile(filepath.Join(r.home, "cron", "jobs.json"))
	if string(before) != string(after) {
		t.Errorf("the file was rewritten on a quiet tick")
	}
	calls := r.turns.taken()
	if len(calls) != 1 || calls[0].job != "good" {
		t.Errorf("turns %+v", calls)
	}
	for id := range bad {
		j := r.job(id)
		if j.Str("state") != "error" || j.Has("next_run_at") && j.Str("next_run_at") != "" || j.Str("last_error") == "" || j.Str("last_run_at") != "" {
			t.Errorf("%s: state %q next %q error %q last run %q", id, j.Str("state"), j.Str("next_run_at"), j.Str("last_error"), j.Str("last_run_at"))
		}
	}
	if out, failed := tools.NewRegistry(r.s.Tool(nil)).Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"run","job_id":"hourly"}`); !failed {
		t.Errorf("run of a job with no next run: %s", out)
	}
	// a fixed schedule recovers
	if err := r.s.mutate(func(f *File) (bool, error) {
		f.Find("hourly").Set("schedule", map[string]any{"kind": "cron", "expr": "0 * * * *", "display": "hourly"})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.tick()
	if j := r.job("hourly"); j.Str("state") != "scheduled" || j.Str("next_run_at") != "2026-10-21T10:00:00Z" {
		t.Errorf("fixed: state %q next %q", j.Str("state"), j.Str("next_run_at"))
	}
}

// start and finish never leave a next run that is not after now: a schedule that went bad
// while the job was due (start) or while it ran (finish) puts it in error with no next run.
func TestStartAndFinishRefuseAStaleNextRun(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, jobJSON("a", `{"kind":"cron","expr":"0 9 L * *"}`, `"2026-10-21T08:00:00Z"`),
		jobJSON("b", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`))
	r.s.start("a", t0) // as if the tick had let it through
	r.s.Wait()
	if c := r.turns.taken(); len(c) != 0 {
		t.Errorf("a ran: %+v", c)
	}
	if j := r.job("a"); j.Str("state") != "error" || j.Str("next_run_at") != "" || j.Bool("running") {
		t.Errorf("a: state %q next %q running %v", j.Str("state"), j.Str("next_run_at"), j.Bool("running"))
	}
	r.turns.reply("b", "done")
	r.turns.onTurn = func() {
		_ = r.s.mutate(func(f *File) (bool, error) {
			f.Find("b").Set("schedule", map[string]any{"kind": "interval", "minutes": 288000000})
			return true, nil
		})
	}
	r.tick()
	if j := r.job("b"); j.Str("last_status") != "ok" || j.Str("state") != "error" || j.Str("next_run_at") != "" || j.Str("last_error") == "" {
		t.Errorf("b: status %q state %q next %q error %q", j.Str("last_status"), j.Str("state"), j.Str("next_run_at"), j.Str("last_error"))
	}
}

// Intervals are bounded (366 days), and a count too long for a number is refused, not read as 0.
func TestIntervalBounds(t *testing.T) {
	for _, s := range []string{"every 200000d", "every 367d", "every 99999999999999999999m", "527041m"} {
		if sch, err := ParseSchedule(s); err == nil {
			t.Errorf("%s accepted: %+v", s, sch)
		}
	}
	for s, m := range map[string]int{"every 366d": 527040, "527040m": 527040} {
		if sch, err := ParseSchedule(s); err != nil || sch.Minutes != m {
			t.Errorf("%s: %+v %v", s, sch, err)
		}
	}
}

// `run` on a job that is running is not lost when the run finishes: the job is due again at once.
func TestRunWhileRunning(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, jobJSON("a", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`))
	reg := tools.NewRegistry(r.s.Tool(nil))
	r.turns.reply("a", "first", "second")
	r.turns.onTurn = func() {
		r.turns.onTurn = nil
		if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"run","job_id":"a"}`); failed {
			t.Errorf("run: %s", out)
		}
	}
	r.tick()
	if j := r.job("a"); j.Str("next_run_at") != t0String || j.Bool("running") {
		t.Fatalf("after the run: next %q running %v", j.Str("next_run_at"), j.Bool("running"))
	}
	r.tick()
	if c := r.turns.taken(); len(c) != 2 {
		t.Errorf("turns %d, want 2", len(c))
	}
	if j := r.job("a"); j.Str("next_run_at") != "2026-10-21T10:00:00Z" {
		t.Errorf("after the requested run: next %q", j.Str("next_run_at"))
	}
}

// A run the shutdown cuts short is "interrupted": not delivered, no failure counted, its status
// kept, and due again at the next start.
func TestShutdownInterruptsRuns(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, strings.Replace(jobJSON("a", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`), `"deliver":"local"`, `"deliver":"slack","last_status":"ok","failure_streak":0`, 1))
	r.turns.block = true
	r.s.o.Tick = time.Hour
	ctx, stopTicks := context.WithCancel(context.Background())
	r.s.Start(ctx)
	for i := 0; i < 200 && !r.job("a").Bool("running"); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if running, _ := r.s.Running(); len(running) != 1 || running[0] != "a" {
		t.Fatalf("running %v", running)
	}
	stopTicks()
	sctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	r.s.Shutdown(sctx)
	cancel()
	j := r.job("a")
	if j.Bool("running") || j.Str("last_status") != "ok" || j.Int("failure_streak") != 0 || j.Str("next_run_at") != t0String || j.Str("last_run_at") != "" {
		t.Errorf("interrupted: running %v status %q streak %d next %q last %q", j.Bool("running"), j.Str("last_status"), j.Int("failure_streak"), j.Str("next_run_at"), j.Str("last_run_at"))
	}
	if d := r.sink.taken(); len(d) != 0 {
		t.Errorf("an interrupted run delivered %+v", d)
	}
	if _, done := j.Repeat(); done != 0 {
		t.Errorf("an interrupted run counted: %d", done)
	}
}
