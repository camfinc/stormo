package sched

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
