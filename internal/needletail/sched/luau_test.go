package sched

import (
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/luau"
)

// A job's .luau script and monitor run in the Luau sandbox (luau.Runtime.RunScript): what they
// print and return is the script's output, a failure is the run's error.
func TestLuauScripts(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, gatedJob("fares", "script", "fares.luau"), gatedJob("watch", "monitor_script", "watch.luau"),
		gatedJob("broken", "script", "broken.luau"))
	eng, err := luau.Shared()
	if err != nil {
		t.Fatal(err)
	}
	lr := &luau.Runtime{Runner: eng, Home: r.home, Grants: []string{"fs.read:home", "fs.write:home"}, ScriptTimeout: 10 * time.Second}
	r.s.o.Luau = lr.RunScript
	writeScript(t, r.home, "fares.luau", "--[[needletail\nformat: 1\n]]\nprint('LIS-JFK 420')\nreturn {cheapest = 'LIS-JFK'}\n")
	writeScript(t, r.home, "watch.luau", "--[[needletail\nformat: 1\n]]\nreturn 'status: green'\n")
	writeScript(t, r.home, "broken.luau", "--[[needletail\nformat: 1\ncaps: [fs.write:home]\n]]\nwrite_file('SOUL.md', 'x')\n")
	r.turns.reply("fares", "Fares noted.")
	r.turns.reply("watch", "All green.")
	r.tick()
	calls := map[string]string{}
	for _, c := range r.turns.taken() {
		calls[c.job] = c.input
	}
	if in := calls["fares"]; !strings.Contains(in, "LIS-JFK 420\n{\"cheapest\":\"LIS-JFK\"}") {
		t.Errorf("fares input:\n%s", in)
	}
	if in := calls["watch"]; !strings.Contains(in, "status: green") {
		t.Errorf("watch input:\n%s", in)
	}
	if in := calls["broken"]; !strings.Contains(in, "protected") {
		t.Errorf("broken input:\n%s", in)
	}
	// without the engine, a .luau script is reported, not run
	r.s.o.Luau = nil
	r.turns.reply("fares", "Noted.")
	r.due("fares")
	r.tick()
	c := r.turns.taken()
	if last := c[len(c)-1]; last.job != "fares" || !strings.Contains(last.input, "Luau is not available") {
		t.Errorf("without the engine: %+v", last)
	}
}
