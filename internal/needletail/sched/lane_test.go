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

// hookSink is a deliverer that runs hook on each delivery (a tool call made meanwhile).
type hookSink struct {
	sink
	hook func(Delivery)
}

func (h *hookSink) Deliver(ctx context.Context, d Delivery) error {
	if h.hook != nil {
		h.hook(d)
	}
	return h.sink.Deliver(ctx, d)
}

// A run asked for while a run finishes (its output being delivered, its outcome being
// recorded) is one more run once it ends, never lost and never an extra one later: the request
// is taken in the same step that lets the job go.
func TestRunAskedWhileFinishing(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, strings.Replace(jobJSON("a", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`), `"deliver":"local"`, `"deliver":"slack"`, 1))
	reg := tools.NewRegistry(r.s.Tool(nil))
	asked := false
	hs := &hookSink{hook: func(Delivery) {
		if !asked {
			asked = true
			if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"run","job_id":"a"}`); failed {
				t.Errorf("run: %s", out)
			}
		}
	}}
	r.s.o.Deliver = hs
	r.turns.reply("a", "first", "second", "third")
	r.tick()
	if j := r.job("a"); j.Str("next_run_at") != t0String {
		t.Errorf("the run asked during the delivery was lost: next %q", j.Str("next_run_at"))
	}
	r.tick()
	r.tick()
	if c := r.turns.taken(); len(c) != 2 {
		t.Errorf("%d turns, want 2", len(c))
	}
	r.s.runMu.Lock()
	left := len(r.s.requested)
	r.s.runMu.Unlock()
	if left != 0 {
		t.Errorf("a request is left over: %v", r.s.requested)
	}
}

// The same for a gated job whose gate found the lane busy: a run asked for as it lets go is the
// due run it already is, not one more later.
func TestRunAskedWhileABusyRunEnds(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, gatedJob("g", "script", "data.sh"))
	writeScript(t, r.home, "data.sh", "echo data\n")
	reg := tools.NewRegistry(r.s.Tool(nil))
	r.s.o.Log = func(l string) {
		if strings.Contains(l, "none is free") {
			if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"run","job_id":"g"}`); failed {
				t.Errorf("run: %s", out)
			}
		}
	}
	r.turns.busy = true
	r.tick()
	r.s.runMu.Lock()
	left := len(r.s.requested)
	r.s.runMu.Unlock()
	if left != 0 {
		t.Errorf("a request is left over from the busy run: %v", r.s.requested)
	}
	r.turns.busy = false
	r.tick()
	r.tick()
	if c := r.turns.taken(); len(c) != 1 {
		t.Errorf("%d turns, want 1", len(c))
	}
}

// Due jobs start oldest first, whatever their order in jobs.json.
func TestDueJobsStartOldestFirst(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, jobJSON("late", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:30:00Z"`),
		jobJSON("early", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`))
	r.turns.lane = 1
	r.tick()
	r.tick()
	c := r.turns.taken()
	if len(c) != 2 || c[0].job != "early" {
		t.Errorf("turns %+v: the older job must go first", c)
	}
}

// A gated job whose gate wants a turn while the lane is busy keeps its gate's result and its
// place: its scripts do not run again tick after tick, and a later job does not take the lane
// before it.
func TestWaitingGateKeepsItsPlace(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	a := gatedJob("a", "script", "count.sh")
	b := strings.Replace(gatedJob("b", "script", "count.sh"), `"2026-10-21T08:00:00Z"`, `"2026-10-21T08:10:00Z"`, 1)
	r := bareRig(t, t0, b, a)
	writeScript(t, r.home, "count.sh", "echo run >> \"$HOME/count\"; echo data\n")
	count := func() int {
		raw, _ := os.ReadFile(filepath.Join(r.home, "count"))
		return strings.Count(string(raw), "run")
	}
	r.turns.lane = 1
	r.turns.busy = true
	r.tick() // both gates run, both find the lane busy
	if n := count(); n != 2 {
		t.Fatalf("%d script runs, want 2", n)
	}
	r.advance(30 * time.Second)
	r.tick()
	r.advance(30 * time.Second)
	r.tick()
	if n := count(); n != 2 {
		t.Errorf("the waiting jobs' scripts ran again: %d runs", n)
	}
	r.turns.busy = false
	r.advance(30 * time.Second)
	r.tick() // a (the older) gets the lane of one; b waits
	r.tick() // then b
	c := r.turns.taken()
	if len(c) != 2 || c[0].job != "a" || c[1].job != "b" || !strings.Contains(c[0].input, "Script Output") || !strings.Contains(c[1].input, "data") {
		t.Errorf("turns %+v", c)
	}
	if n := count(); n != 2 {
		t.Errorf("the scripts ran again before the turns: %d runs", n)
	}
	// a later job whose gate ends while an older one waits leaves the lane to the older one
	r.turns.busy = true
	r.due("a")
	r.tick() // a: gate, busy: waits with its result
	r.turns.busy = false
	if err := r.s.mutate(func(f *File) (bool, error) {
		f.Find("b").Set("next_run_at", r.s.at(r.s.o.Now().Add(time.Second)))
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	r.s.start("b", r.s.o.Now().Add(time.Second))
	r.s.Wait()
	if c := r.turns.taken(); len(c) != 0 {
		t.Errorf("b took the lane while a waited: %+v", c)
	}
	r.advance(2 * time.Second)
	r.tick()
	r.tick()
	if c := r.turns.taken(); len(c) != 2 || c[0].job != "a" || c[1].job != "b" {
		t.Errorf("after: %+v", c)
	}
}

// A waiting job's gate result is kept until its schedule's next time: past it the gate runs
// again (its data may be stale).
func TestWaitingGateExpires(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, gatedJob("a", "script", "count.sh"))
	writeScript(t, r.home, "count.sh", "echo run >> \"$HOME/count\"; echo data\n")
	count := func() int {
		raw, _ := os.ReadFile(filepath.Join(r.home, "count"))
		return strings.Count(string(raw), "run")
	}
	r.turns.busy = true
	r.tick()
	for i := 0; i < 4; i++ {
		r.advance(10 * time.Minute)
		r.tick()
	}
	if n := count(); n != 1 {
		t.Errorf("%d script runs within the hour, want 1", n)
	}
	r.advance(30 * time.Minute) // past the schedule's next time
	r.tick()
	if n := count(); n != 2 {
		t.Errorf("%d script runs after the hour, want 2", n)
	}
}
