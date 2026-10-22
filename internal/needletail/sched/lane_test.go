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

// A `run` on a job already in line keeps its place: the line moves on (here, the jobs' lane of
// one is free again and both waiting jobs run, oldest first), never stalls with each job seeing
// the other ahead of it, and the job's due time does not move back.
func TestRunOnAWaitingJobKeepsTheLineMoving(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	a := gatedJob("a", "script", "data.sh")
	b := strings.Replace(gatedJob("b", "script", "data.sh"), `"2026-10-21T08:00:00Z"`, `"2026-10-21T08:10:00Z"`, 1)
	r := bareRig(t, t0, b, a)
	writeScript(t, r.home, "data.sh", "echo data\n")
	reg := tools.NewRegistry(r.s.Tool(nil))
	r.turns.lane = 1
	r.turns.busy = true
	r.tick() // both gates run, both wait in line
	if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"run","job_id":"a"}`); failed {
		t.Fatal(out)
	}
	if got := r.job("a").Str("next_run_at"); got != "2026-10-21T08:00:00Z" {
		t.Errorf("run moved a due job back: next %q", got)
	}
	r.turns.busy = false
	r.advance(30 * time.Second)
	r.tick()
	r.tick()
	if c := r.turns.taken(); len(c) != 2 || c[0].job != "a" || c[1].job != "b" {
		t.Errorf("turns %+v: the line stalled", c)
	}
}

// A waiting job whose kept gate result expired keeps its place in line while its gate runs
// again: a later job does not take the lane before it.
func TestExpiredGateKeepsItsPlace(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	a := gatedJob("a", "script", "slow.sh")
	b := jobJSON("b", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:10:00Z"`)
	r := bareRig(t, t0, a, b)
	writeScript(t, r.home, "slow.sh", "sleep 0.3; echo data\n")
	r.turns.lane = 1
	r.turns.busy = true
	r.tick()                    // a: gate, busy, waits with its result; b: busy, waits
	r.advance(61 * time.Minute) // past a's next time: its kept result is stale
	r.turns.busy = false
	r.tick() // a's gate runs again; b must not take the lane meanwhile
	r.tick()
	if c := r.turns.taken(); len(c) != 2 || c[0].job != "a" || c[1].job != "b" {
		t.Errorf("turns %+v: the later job went first", c)
	}
}

// A job changed while its gate runs does not get that gate's result back: the update's change is
// not mixed with output its old definition made.
func TestUpdateDuringTheGateDropsItsResult(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	// an agent's own job (created_at stamped): its script can be updated
	r := bareRig(t, t0, strings.Replace(gatedJob("a", "script", "old.sh"), `"deliver":"local"`, `"deliver":"local","created_at":"2026-10-20T09:00:00Z"`, 1))
	writeScript(t, r.home, "old.sh", "sleep 0.3; echo old output\n")
	writeScript(t, r.home, "new.sh", "echo new output\n")
	reg := tools.NewRegistry(r.s.Tool(nil))
	r.turns.busy = true
	r.s.Tick()
	time.Sleep(100 * time.Millisecond)
	if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"update","job_id":"a","script":"new.sh"}`); failed {
		t.Fatal(out)
	}
	r.s.Wait()
	r.turns.busy = false
	r.due("a")
	r.tick()
	c := r.turns.taken()
	if len(c) != 1 || strings.Contains(c[0].input, "old output") || !strings.Contains(c[0].input, "new output") {
		t.Errorf("turns %+v", c)
	}
}
