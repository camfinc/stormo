package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fareTool is the Luau tool the agent writes: it reads the fare table a script left in the home,
// writes a report and returns the fare.
const fareTool = `--[[needletail
format: 1
name: fare_quote
description: Quote a route's fare from the fare table, in EUR.
args: {type: object, properties: {route: {type: string}}, required: [route]}
caps: [fs.read:home, fs.write:home/reports/**]
timeout: 10s
]]
return function(args)
  local table_ = json_decode(read_file("data/fares.json"))
  local fare = table_[args.route]
  if not fare then error("no fare for " .. args.route) end
  append_file("reports/quotes.md", args.route .. ": " .. fare .. " EUR\n")
  log("info", "quoted " .. args.route)
  return {route = args.route, fare = fare, currency = "EUR"}
end
`

// grantReports adds the write grant the fare tool needs to the rendered needletail.yaml: the
// default write grant is the conversation's work/ only (fs.write:home/work/**), so a tool that
// writes reports/ is granted that directory, as an agent's engine/needletail/config.yaml would.
func grantReports(t *testing.T, home string) {
	t.Helper()
	p := filepath.Join(home, "needletail.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	const def = "        - fs.write:home/work/**\n"
	if !strings.Contains(string(raw), def) {
		t.Fatalf("needletail.yaml has no default write grant:\n%s", raw)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(raw), def, def+"        - fs.write:home/reports/**\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// M6's done-when (docs/needletail.md §9): an agent writes a Luau tool through skill_manage, calls
// it the next turn, and the dream proposes it. The real build, rehydrate, oneshot turns (driven
// by a scripted model), nap and dream, as TestNapRehydrateDream; then a second boot from the nap
// gets the tool back.
func TestLuauToolLifecycle(t *testing.T) {
	w := newLearningWorld(t)
	home := filepath.Join(w.tmp, "home1")
	w.rehydrate(home)
	grantReports(t, home)
	if err := os.MkdirAll(filepath.Join(home, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "data", "fares.json"), []byte(`{"LIS-JFK": 420, "OPO-LHR": 95}`), 0o644); err != nil {
		t.Fatal(err)
	}
	skillMD := "---\nname: fare-lookup\ndescription: Quote fares from the fare table.\n---\nCall fare-lookup__fare_quote with the route.\n"

	// Turn 1: the agent makes the skill and its tool. A broken tool is refused with its error, the
	// new tool is not offered in the turn that wrote it, and luau_test dry-runs it (its write is
	// refused there).
	w.model.script(
		toolStep("c1", "skill_manage", map[string]any{"action": "create", "name": "fare-lookup", "category": "crm", "content": skillMD}),
		toolStep("c2", "skill_manage", map[string]any{"action": "write_file", "name": "fare-lookup", "file_path": "tools/fare_quote.luau",
			"file_content": strings.Replace(fareTool, "return {route", "return {route =", 1)}),
		toolStep("c3", "skill_manage", map[string]any{"action": "write_file", "name": "fare-lookup", "file_path": "tools/fare_quote.luau", "file_content": fareTool}),
		toolStep("c4", "fare-lookup__fare_quote", map[string]any{"route": "LIS-JFK"}),
		toolStep("c5", "luau_test", map[string]any{"tool": "fare-lookup__fare_quote", "args": map[string]any{"route": "LIS-JFK"}}),
		toolStep("c6", "skill_view", map[string]any{"name": "fare-lookup"}),
		textStep("Done."),
	)
	tools := w.oneshot(home, "Make a tool that quotes fares.")
	if want := []string{"skill_manage", "skill_manage", "skill_manage", "fare-lookup__fare_quote", "luau_test", "skill_view"}; !slices.Equal(tools, want) {
		t.Errorf("turn 1 tools = %v", tools)
	}
	seen := w.model.seen
	if len(seen) < 7 {
		t.Fatalf("seen %d", len(seen))
	}
	for i, want := range []string{
		"syntax:",                    // c2: the broken tool is refused
		`\"success\":true`,           // c3: written
		`not available in this turn`, // c4: not offered in the turn that wrote it
		`dry_run`,                    // c5: the dry run refuses its write
		`fare-lookup__fare_quote`,    // c6: the view lists the tool
	} {
		if !strings.Contains(seen[i+2], want) {
			t.Errorf("result of c%d lacks %q:\n%s", i+2, want, seen[i+2])
		}
	}
	if !strings.Contains(seen[6], `\"effective_caps\":[\"fs.read:home\",\"fs.write:home/reports/**\"]`) {
		t.Errorf("the view shows no caps:\n%s", seen[6])
	}
	if _, err := os.Stat(filepath.Join(home, "reports", "quotes.md")); err == nil {
		t.Fatal("the dry run wrote")
	}

	// Turn 2: the tool is offered and runs.
	w.model.script(
		toolStep("d1", "fare-lookup__fare_quote", map[string]any{"route": "LIS-JFK"}),
		textStep("Done."),
	)
	var stdout, stderr bytes.Buffer
	setenv(t, home)
	if code := Run(context.Background(), []string{"oneshot", "--home", home, "--format", "stream-json", "-q", "Quote LIS-JFK."}, &stdout, &stderr); code != 0 {
		t.Fatalf("turn 2: exit %d: %s", code, stderr.String())
	}
	// observers (the bench's stream, the core's hooks) see it as a Luau tool
	if !strings.Contains(stdout.String(), `"name":"luau:fare-lookup__fare_quote"`) {
		t.Errorf("stream: %s", stdout.String())
	}
	if res := w.model.seen[1]; !strings.Contains(res, `\"fare\":420`) || !strings.Contains(res, `quoted LIS-JFK`) {
		t.Errorf("turn 2 result: %s", res)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "reports", "quotes.md")); string(b) != "LIS-JFK: 420 EUR\n" {
		t.Errorf("report = %q", b)
	}

	// Nap and dream: the tool is learning, the audit raw, and the skill is proposed with its tool.
	n := w.nap(home, "nt-1", time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC))
	classes := map[string]string{}
	for _, f := range n.Files {
		classes[f.Path] = string(f.Class)
	}
	if classes["skills/crm/fare-lookup/tools/fare_quote.luau"] != "learning" {
		t.Errorf("the tool is %q in the nap", classes["skills/crm/fare-lookup/tools/fare_quote.luau"])
	}
	audits := 0
	for p, c := range classes {
		if strings.HasPrefix(p, "logs/audit/") {
			audits++
			if c != "raw" {
				t.Errorf("%s is %s", p, c)
			}
		}
	}
	if audits == 0 {
		t.Error("no audit log in the nap")
	}
	r, _ := w.dream()
	proposed := false
	for _, p := range r.SkillProposals {
		if p.Skill == "crm/fare-lookup" && p.Status == "new" {
			proposed = true
		}
	}
	if !proposed {
		t.Fatalf("proposals = %+v", r.SkillProposals)
	}
	prop := filepath.Join(w.learnings, "proposals", "skills", "crm", "fare-lookup", "tools", "fare_quote.luau")
	if b, err := os.ReadFile(prop); err != nil || string(b) != fareTool {
		t.Fatalf("proposed tool = %q %v", b, err)
	}

	// A second boot from the nap has the tool again.
	home2 := filepath.Join(w.tmp, "home2")
	w.rehydrate(home2)
	grantReports(t, home2)
	if err := os.MkdirAll(filepath.Join(home2, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home2, "data", "fares.json"), []byte(`{"OPO-LHR": 95}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w.model.script(toolStep("e1", "fare-lookup__fare_quote", map[string]any{"route": "OPO-LHR"}), textStep("Done."))
	if tools := w.oneshot(home2, "Quote OPO-LHR."); !slices.Equal(tools, []string{"luau:fare-lookup__fare_quote"}) {
		t.Errorf("second boot tools = %v", tools)
	}
	if res := w.model.seen[1]; !strings.Contains(res, `\"fare\":95`) {
		t.Errorf("second boot result: %s", res)
	}
}

func TestLuauCheckCommand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills", "crm", "fare-lookup", "tools")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	good, bad, script := filepath.Join(dir, "fare_quote.luau"), filepath.Join(dir, "broken.luau"), filepath.Join(t.TempDir(), "job.luau")
	_ = os.WriteFile(good, []byte(fareTool), 0o644)
	_ = os.WriteFile(bad, []byte(strings.Replace(fareTool, "name: fare_quote", "name: broken", 1)+"return {"), 0o644)
	_ = os.WriteFile(script, []byte("--[[needletail\nformat: 1\n]]\nreturn 1\n"), 0o644)
	hdir := filepath.Join(filepath.Dir(dir), "handlers")
	_ = os.MkdirAll(hdir, 0o755)
	handler, toolAsHandler := filepath.Join(hdir, "greet.luau"), filepath.Join(hdir, "fare_quote.luau")
	_ = os.WriteFile(handler, []byte("--[[needletail\nformat: 1\nname: greet\ndescription: Greets.\nevents: [on_message]\n]]\nfunction on_message(ev) return nil end\n"), 0o644)
	_ = os.WriteFile(toolAsHandler, []byte(fareTool), 0o644) // a tool's header (args, no events) in handlers/
	for file, want := range map[string]int{good: 0, bad: 1, script: 0, filepath.Join(dir, "missing.luau"): 1, handler: 0, toolAsHandler: 1} {
		var stdout, stderr bytes.Buffer
		if code := Run(context.Background(), []string{"luau", "check", file}, &stdout, &stderr); code != want {
			t.Errorf("%s: exit %d, want %d: %s%s", filepath.Base(file), code, want, stdout.String(), stderr.String())
		}
	}
	var stdout bytes.Buffer
	if code := Run(context.Background(), []string{"luau", "reference"}, &stdout, &bytes.Buffer{}); code != 0 || !strings.Contains(stdout.String(), "nt.fs.read(path) · `read_file(path)`") {
		t.Errorf("reference: %d %s", code, stdout.String())
	}
}
