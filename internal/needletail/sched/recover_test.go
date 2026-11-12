package sched

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// flakyScript fails as a script cut short by its source does, counting its runs in $HOME/runs.
const flakyScript = `echo run >> "$HOME/runs"; echo "http.client.IncompleteRead: IncompleteRead(57337 bytes read)" >&2; exit 1` + "\n"

func flakyJob(id string, streak int) string {
	j := strings.Replace(jobJSON(id, `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`), `"prompt":"Report."`, `"no_agent":true,"script":"flaky.sh"`, 1)
	return strings.Replace(j, `"deliver":"local"`, fmt.Sprintf(`"deliver":"slack","failure_streak":%d`, streak), 1)
}

func scriptRuns(t *testing.T, home string) int {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(home, "runs"))
	return strings.Count(string(b), "run\n")
}

// A script-only job whose script fails gets a recovery turn instead of the raw failure being
// posted (docs/schedules.md §3): what the agent replies once it set the run right is the job's
// output, [SILENT] keeps it quiet, and the run's record keeps the failure. Once the job has failed
// failureNudgeAt runs in a row, or with no agent to run it, the failure is posted as before.
func TestScriptRecovery(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, flakyJob("fixed", 0), flakyJob("quiet", 1), flakyJob("spent", failureNudgeAt))
	writeScript(t, r.home, "flaky.sh", flakyScript)
	r.turns.reply("fixed", "collected 3 accounts")
	r.turns.reply("quiet", "[SILENT]")
	r.tick()

	calls := map[string]turnCall{}
	for _, c := range r.turns.taken() {
		calls[c.job] = c
	}
	if _, ok := calls["spent"]; ok || len(calls) != 2 {
		t.Fatalf("turns %v, want fixed and quiet", calls)
	}
	for _, w := range []string{"## Script Error", "IncompleteRead(57337 bytes read)", "This run is a recovery", "cd scripts && bash flaky.sh",
		"reply with exactly what it printed", "[CRON_FAILURE] alone"} {
		if !strings.Contains(calls["fixed"].input, w) {
			t.Errorf("the recovery prompt lacks %q:\n%s", w, calls["fixed"].input)
		}
	}
	for id, want := range map[string]string{"fixed": `ok streak=0 err=""`, "quiet": `silent streak=0 err=""`} {
		if got := r.state(id); got != want {
			t.Errorf("%s: %s, want %s", id, got, want)
		}
	}
	if got := r.state("spent"); !strings.HasPrefix(got, fmt.Sprintf("error streak=%d err=\"Script exited with code 1", failureNudgeAt+1)) {
		t.Errorf("spent: %s", got)
	}
	got := map[string]Delivery{}
	for _, d := range r.sink.taken() {
		got[d.JobID] = d
	}
	if d, ok := got["fixed"]; !ok || d.Failure || d.Text != "collected 3 accounts" {
		t.Errorf("fixed delivered %+v", d)
	}
	if _, ok := got["quiet"]; ok {
		t.Errorf("a silent recovery was delivered: %+v", got["quiet"])
	}
	if d := got["spent"]; !d.Failure || !strings.Contains(d.Text, "script failed") || !strings.Contains(d.Text, "IncompleteRead") || !strings.Contains(d.Text, "worth a review") {
		t.Errorf("spent delivered %+v", d)
	}
	recs, _ := filepath.Glob(filepath.Join(r.home, "state", "needletail", "jobs", "fixed", "*.md"))
	if len(recs) != 1 {
		t.Fatalf("fixed records %v", recs)
	}
	if body, _ := os.ReadFile(recs[0]); !strings.Contains(string(body), "IncompleteRead") || !strings.Contains(string(body), "collected 3 accounts") {
		t.Errorf("the record lacks the failure or the reply:\n%s", body)
	}
	if n := scriptRuns(t, r.home); n != 3 {
		t.Errorf("the scheduler ran the script %d times, want once per job", n)
	}

	// no agent to run a recovery: the failure is posted as it is
	bare := bareRig(t, t0, flakyJob("alone", 0))
	writeScript(t, bare.home, "flaky.sh", flakyScript)
	bare.s.SetTurns(nil)
	bare.tick()
	if d := bare.sink.taken(); len(d) != 1 || !d[0].Failure || !strings.Contains(d[0].Text, "script failed") {
		t.Errorf("with no agent: delivered %+v", d)
	}
}

// The rerun the agent is told to try runs the script as the scheduler does: from its own
// directory, with the interpreter its extension picks.
func TestRerunHint(t *testing.T) {
	for script, want := range map[string]string{
		"collect.py": "`cd scripts && python3 collect.py`", "social/pull.py": "`cd scripts/social && python3 pull.py`",
		"watch.sh": "`cd scripts && bash watch.sh`", "probe.luau": "scheduler's sandbox",
	} {
		if got := rerunHint(script); !strings.Contains(got, want) {
			t.Errorf("%s: %s", script, got)
		}
	}
}

// A recovery that finds the jobs' lane busy waits in line with its gate's result, as an agent
// job's gate does: the script does not run again every tick, and the turn gets the failure once
// a slot is free.
func TestScriptRecoveryWaitsForTheLane(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, flakyJob("wait", 0))
	writeScript(t, r.home, "flaky.sh", flakyScript)
	r.turns.busy = true
	r.tick()
	r.advance(30 * time.Second)
	r.tick()
	if j := r.job("wait"); j.Str("last_run_at") != "" || j.Str("next_run_at") != "2026-10-21T08:00:00Z" {
		t.Errorf("busy: last %q next %q", j.Str("last_run_at"), j.Str("next_run_at"))
	}
	if d := r.sink.taken(); len(d) != 0 {
		t.Errorf("delivered while waiting: %+v", d)
	}
	r.turns.busy = false
	r.turns.reply("wait", "[CRON_FAILURE]\nThe source keeps cutting its responses short; it needs a look.")
	r.advance(30 * time.Second)
	r.tick()
	c := r.turns.taken()
	if len(c) != 1 || !strings.Contains(c[0].input, "IncompleteRead") || !strings.Contains(c[0].input, "This run is a recovery") {
		t.Fatalf("turns %+v", c)
	}
	if n := scriptRuns(t, r.home); n != 1 {
		t.Errorf("the script ran %d times while the job waited, want 1", n)
	}
	if got := r.state("wait"); got != `error streak=1 err="The source keeps cutting its responses short; it needs a look."` {
		t.Errorf("after the lane: %s", got)
	}
	if d := r.sink.taken(); len(d) != 1 || !d[0].Failure || !strings.Contains(d[0].Text, "it needs a look") || strings.Contains(d[0].Text, "IncompleteRead") {
		t.Errorf("delivered %+v", d)
	}
}
