package sched

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/tools"
)

func writeScript(t *testing.T, home, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "scripts", name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func gatedJob(id, key, script string) string {
	return strings.Replace(jobJSON(id, `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`), `"prompt":"Report."`, `"prompt":"Report.","`+key+`":"`+script+`"`, 1)
}

// A monitor or pre-run script runs before the job takes a turn slot: an unchanged monitor is a
// no_change run that never takes one, and while the agent is busy a changed output is not
// stored, so the next tick still sees the change and runs the agent on it.
func TestGateBeforeSlot(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	sum := sha256.Sum256([]byte("same"))
	same := strings.Replace(gatedJob("same", "monitor_script", "same.sh"), `"deliver":"local"`,
		`"deliver":"local","monitor_state":{"last_output_hash":"`+hex.EncodeToString(sum[:])+`","last_changed_at":"2026-10-20T09:00:00Z"}`, 1)
	r := bareRig(t, t0, same, gatedJob("new", "monitor_script", "new.sh"), gatedJob("quiet", "script", "quiet.sh"))
	writeScript(t, r.home, "same.sh", "echo same\n")
	writeScript(t, r.home, "new.sh", "echo fresh output\n")
	writeScript(t, r.home, "quiet.sh", "echo '{\"wakeAgent\": false}'\n")
	r.turns.busy = true
	r.tick()
	if got := r.job("same").Str("last_status"); got != "no_change" {
		t.Errorf("same: %q", got)
	}
	if got := r.job("quiet").Str("last_status"); got != "silent" {
		t.Errorf("quiet: %q", got)
	}
	n := r.job("new")
	if n.Str("last_status") != "" || n.Bool("running") || n.Str("next_run_at") != "2026-10-21T08:00:00Z" || n.Has("monitor_state") && string(n.raw["monitor_state"]) != "null" {
		t.Errorf("new while busy: status %q running %v next %q monitor %s", n.Str("last_status"), n.Bool("running"), n.Str("next_run_at"), n.raw["monitor_state"])
	}
	if _, err := os.Stat(filepath.Join(r.home, "state", "needletail", "monitor", "new.out")); err == nil {
		t.Error("the snapshot was stored while no turn ran")
	}
	if r.turns.reserved != 0 {
		t.Errorf("%d slots reserved", r.turns.reserved)
	}
	r.turns.busy = false
	r.turns.reply("new", "It changed.")
	r.advance(30 * time.Second)
	r.tick()
	c := r.turns.taken()
	if len(c) != 1 || c[0].job != "new" || !strings.Contains(c[0].input, "Monitor Baseline") || r.job("new").Str("last_status") != "ok" {
		t.Errorf("after busy: calls %+v status %q", c, r.job("new").Str("last_status"))
	}
	if _, ok := r.job("new").monitor(); !ok {
		t.Error("the hash was not stored after the turn")
	}
}

// Gates run under their own cap of two, holding no turn slot while they run: a third gated job
// waits for one of them.
func TestGateCap(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, gatedJob("g1", "script", "slow.sh"), gatedJob("g2", "script", "slow.sh"), gatedJob("g3", "script", "slow.sh"))
	writeScript(t, r.home, "slow.sh", "sleep 1; echo data\n")
	started := time.Now()
	r.s.Tick()
	time.Sleep(300 * time.Millisecond)
	r.turns.mu.Lock()
	held := r.turns.held
	r.turns.mu.Unlock()
	if held != 0 {
		t.Errorf("%d slots held during the scripts", held)
	}
	r.s.Wait()
	if took := time.Since(started); took < 2*time.Second {
		t.Errorf("three 1 s scripts took %s: more than two ran at once", took)
	}
	if c := r.turns.taken(); len(c) != 3 {
		t.Errorf("%d turns", len(c))
	}
}

// A gate that wants a turn while the jobs' lane is busy leaves the job for the next tick: no kick
// (which would run its scripts again at once, in a loop, until a turn is free), and a run asked
// for meanwhile is taken, not left to fire later.
func TestBusyGateWaitsForTheTick(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, gatedJob("new", "monitor_script", "new.sh"))
	writeScript(t, r.home, "new.sh", "sleep 0.3; echo fresh output\n")
	r.turns.busy = true
	reg := tools.NewRegistry(r.s.Tool(nil))
	r.s.Tick()
	time.Sleep(100 * time.Millisecond)
	if running, _ := r.s.Running(); len(running) != 1 {
		t.Fatalf("the gate is not running when the run is asked for: %v", running)
	}
	if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"run","job_id":"new"}`); failed {
		t.Fatal(out)
	}
	select {
	case <-r.s.kick: // the tool's own kick
	default:
	}
	r.s.Wait()
	if n := len(r.s.kick); n != 0 {
		t.Error("a busy outcome kicked the ticker")
	}
	r.s.runMu.Lock()
	left := len(r.s.requested)
	r.s.runMu.Unlock()
	if left != 0 {
		t.Errorf("the run asked during a busy run is still pending: %v", r.s.requested)
	}
}

// A job never runs twice at once: a tick while it runs leaves it alone, and runs asked for
// while it runs coalesce into one more run once it ends.
func TestSingleFlightAndCoalescedRuns(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, jobJSON("solo", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`))
	release := make(chan struct{})
	var once sync.Once
	r.turns.onTurn = func() { once.Do(func() { <-release }) }
	reg := tools.NewRegistry(r.s.Tool(nil))
	r.s.Tick()
	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if out, failed := reg.Call(context.Background(), &tools.Env{}, CronJobTool, `{"action":"run","job_id":"solo"}`); failed {
			t.Fatal(out)
		}
		r.s.Tick() // due now, but running: not started again
	}
	if running, _ := r.s.Running(); len(running) != 1 {
		t.Errorf("running %v", running)
	}
	close(release)
	r.s.Wait()
	r.tick() // the one run the three requests asked for
	r.tick() // nothing more
	if c := r.turns.taken(); len(c) != 2 {
		t.Errorf("%d turns, want 2", len(c))
	}
}
