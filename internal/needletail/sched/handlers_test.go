package sched

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// on_schedule handlers run after a job's monitor and pre-run script, before its turn: a reply is
// the run's output with no turn (the monitor's change counts as handled), silent a silent run,
// a pass's notes go into the prompt. They see the job and the script's output.
func TestOnSchedule(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, jobJSON("plain", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`),
		gatedJob("mon", "monitor_script", "mon.sh"), gatedJob("noted", "script", "data.sh"), jobJSON("hush", `{"kind":"interval","minutes":60}`, `"2026-10-21T08:00:00Z"`))
	writeScript(t, r.home, "mon.sh", "echo changed\n")
	writeScript(t, r.home, "data.sh", "echo 3 rows\n")
	seen := map[string]map[string]any{}
	r.s.o.OnSchedule = func(_ context.Context, id, name string, ev map[string]any) messaging.Verdict {
		r.mu.Lock()
		seen[id] = ev
		r.mu.Unlock()
		switch id {
		case "mon":
			return messaging.Verdict{Action: "reply", Text: "Handled by Luau.", By: "ops__mon"}
		case "hush":
			return messaging.Verdict{Action: "silent", By: "ops__hush"}
		case "noted":
			return messaging.Verdict{Action: "pass", Context: []string{"rows look fine"}}
		}
		return messaging.Verdict{Action: "pass"}
	}
	r.turns.reply("plain", "Plain run.")
	r.turns.reply("noted", "Noted run.")
	r.tick()
	for id, want := range map[string]string{"plain": "ok", "mon": "ok", "noted": "ok", "hush": "silent"} {
		if got := r.job(id).Str("last_status"); got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	calls := map[string]turnCall{}
	for _, c := range r.turns.taken() {
		calls[c.job] = c
	}
	if _, ok := calls["mon"]; ok || len(calls) != 2 {
		t.Errorf("turns %+v (the handler answered mon, hush is silent)", calls)
	}
	if !strings.Contains(calls["noted"].input, "## Handler Notes\nYour on_schedule handlers noted this for the run.\n\n```\nrows look fine\n```") {
		t.Errorf("noted input %q", calls["noted"].input)
	}
	if _, ok := r.job("mon").monitor(); !ok {
		t.Error("the monitor's output was not stored when the handler answered")
	}
	if _, err := os.Stat(filepath.Join(r.home, "state", "needletail", "monitor", "mon.out")); err != nil {
		t.Error(err)
	}
	ev := seen["noted"]
	job, _ := ev["job"].(map[string]any)
	if ev["script_output"] != "3 rows" || job["id"] != "noted" || job["prompt"] != "Report." || job["schedule"] != "every 60m" || job["repo"] != true {
		t.Errorf("event %+v", ev)
	}
	if rc, _ := seen["mon"]["run_context"].(string); !strings.Contains(rc, "changed") {
		t.Errorf("mon's run context %q", rc)
	}
}

// A job whose gate (its script, then its on_schedule handlers) wants a turn while the lane is
// busy keeps the handlers' verdict and notes with its gate's result: the handlers run once per
// due run, not again at every tick it waits (a handler that sends, then passes, would send each
// time), and their notes reach the turn it finally gets.
func TestBusyRunKeepsHandlerVerdict(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, gatedJob("g", "script", "data.sh"))
	writeScript(t, r.home, "data.sh", "echo rows\n")
	calls := 0
	r.s.o.OnSchedule = func(_ context.Context, id, name string, ev map[string]any) messaging.Verdict {
		r.mu.Lock()
		calls++
		r.mu.Unlock()
		return messaging.Verdict{Action: "pass", Context: []string{"sent the heads-up"}}
	}
	r.turns.reply("g", "Done.")
	r.turns.busy = true
	r.tick()
	for i := 0; i < 3; i++ {
		r.advance(10 * time.Minute)
		r.tick()
	}
	r.turns.busy = false
	r.advance(time.Minute)
	r.tick()
	r.mu.Lock()
	n := calls
	r.mu.Unlock()
	if n != 1 {
		t.Errorf("the handlers ran %d times for one due run", n)
	}
	c := r.turns.taken()
	if len(c) != 1 || !strings.Contains(c[0].input, "sent the heads-up") {
		t.Errorf("turns %+v", c)
	}
}
