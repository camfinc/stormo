package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/pkg/build"
	adapter "github.com/camfinc/stormo/pkg/engine/needletail"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
)

// The skills library reads the booted set where rehydrate writes it.
func TestBootedFile(t *testing.T) {
	if skills.BootedFile != loop.BootedPath {
		t.Errorf("skills.BootedFile = %s, rehydrate writes %s", skills.BootedFile, loop.BootedPath)
	}
}

// scripted is an OpenAI-compatible endpoint that answers each request with the next step of a
// script: a tool call, or the final text.
type scripted struct {
	mu    sync.Mutex
	steps []string
	seen  []string // the last row of each request: what the runtime sent back
}

func toolStep(id, name string, args any) string {
	b, _ := json.Marshal(args)
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"tool_calls": []any{map[string]any{"index": 0, "id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(b)}}},
	}, "finish_reason": "tool_calls"}}})
	return string(frame)
}

func textStep(text string) string {
	frame, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": text}, "finish_reason": "stop"}}})
	return string(frame)
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/chat/completions" {
		http.NotFound(w, r)
		return
	}
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	s.mu.Lock()
	step := textStep("(script ended)")
	if len(s.steps) > 0 {
		step, s.steps = s.steps[0], s.steps[1:]
	}
	if n := len(req.Messages); n > 0 {
		s.seen = append(s.seen, string(req.Messages[n-1]))
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\n", step)
	fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110}}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (s *scripted) script(steps ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps, s.seen = steps, nil
}

// spyStore records every key the dream reads.
type spyStore struct {
	loop.Store
	mu    sync.Mutex
	reads []string
}

func (s *spyStore) Get(k string) ([]byte, error) {
	s.mu.Lock()
	s.reads = append(s.reads, k)
	s.mu.Unlock()
	return s.Store.Get(k)
}

func readJSON(t *testing.T, p string, v any) {
	t.Helper()
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

// learningWorld is the example instance's atlas agent on needletail, built for real, with a
// scripted model and a nap store: what TestNapRehydrateDream and TestLuauToolLifecycle run on.
type learningWorld struct {
	model     *scripted
	root      string
	tmp       string
	learnings string
	rehydrate func(home string) *loop.RehydrateReport
	oneshot   func(home, prompt string) []string
	nap       func(home, instance string, at time.Time) *loop.Nap
	dream     func() (*loop.DreamReport, []string)
}

func newLearningWorld(t *testing.T) *learningWorld {
	t.Helper()
	model := &scripted{}
	srv := httptest.NewServer(model)
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	root := filepath.Join(tmp, "instance")
	if err := os.CopyFS(root, os.DirFS(filepath.Join("..", "..", "..", "examples", "minimal"))); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(root, "agents", "atlas", "learnings"))
	manifestPath := filepath.Join(root, "agents", "atlas", "agent.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	m := string(raw)
	for old, repl := range map[string]string{
		"  kind: hermes\n  version: 0.21.5\n  image_tag: v2026.9.24\n": "  kind: needletail\n  version: 0.1.0\n  image_tag: v0.1.0\n",
		"  provider: openrouter\n":                                     "  provider: stub\n",
	} {
		if !strings.Contains(m, old) {
			t.Fatalf("agent.yaml has no %q", old)
		}
		m = strings.Replace(m, old, repl, 1)
	}
	if err := os.WriteFile(manifestPath, []byte(m), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(root, "stormo.yaml"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(f, "\nconnections:\n  - {name: stub, kind: custom, base_url: %s/v1, key: OPENROUTER_API_KEY}\n", srv.URL)
	f.Close()
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := manifest.Load(root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	eng := adapter.New(inst)
	base := filepath.Join(tmp, "baseline")
	if _, err := build.Agent(inst, "atlas", build.Options{Out: base, Target: manifest.AWS}); err != nil {
		t.Fatal(err)
	}
	store := loop.FsStore{Root: filepath.Join(tmp, "store")}
	scrubber := learning.NewScrubber(inst)
	rehydrate := func(home string) *loop.RehydrateReport {
		t.Helper()
		r, err := loop.Rehydrate(loop.RehydrateOptions{Agent: agent, Engine: eng, Home: home, BaselineDir: base, Store: store, Scrubber: scrubber})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	oneshot := func(home, prompt string) []string {
		t.Helper()
		setenv(t, home)
		var stdout, stderr bytes.Buffer
		code := Run(context.Background(), []string{"oneshot", "--home", home, "--format", "stream-json", "-q", prompt}, &stdout, &stderr)
		run := eng.ParseBench(stdout.String(), code)
		if code != 0 || run.Error != "" || run.Text != "Done." {
			t.Fatalf("oneshot exit %d: %+v\nstderr: %s", code, run, stderr.String())
		}
		return run.Tools
	}
	nap := func(home, instance string, at time.Time) *loop.Nap {
		t.Helper()
		n, err := loop.NapOnce(loop.NapOptions{Agent: "atlas", Unit: agent.Unit, Engine: eng, Home: home, Store: store, Instance: instance, BaselineDir: base, Now: at})
		if err != nil || n == nil {
			t.Fatalf("nap = %v %v", n, err)
		}
		return n
	}
	dream := func() (*loop.DreamReport, []string) {
		t.Helper()
		spy := &spyStore{Store: store}
		r, err := loop.Dream(inst, "atlas", spy)
		if err != nil {
			t.Fatal(err)
		}
		return r, spy.reads
	}
	return &learningWorld{model: model, root: root, tmp: tmp, learnings: filepath.Join(root, "agents", "atlas", "learnings"),
		rehydrate: rehydrate, oneshot: oneshot, nap: nap, dream: dream}
}

// M3's done-when (docs/needletail.md §9): a nap, rehydrate and dream cycle on a needletail home
// yields ledger sightings and a skill proposal from an agent-created skill, with the learning
// loop's code (pkg/loop) unchanged. Everything here is the real code: `stormo build` of the
// example atlas agent on needletail, rehydrate into a home, the runtime's oneshot turn (its tools
// driven by a scripted model), then the nap, the dream, and a second boot from the nap.
func TestNapRehydrateDream(t *testing.T) {
	w := newLearningWorld(t)
	model, root, tmp, rehydrate, oneshot, nap, dream := w.model, w.root, w.tmp, w.rehydrate, w.oneshot, w.nap, w.dream
	learnings := w.learnings

	// 1. First boot: the baseline alone. The generated skills reach the runtime as read-only.
	home1 := filepath.Join(tmp, "home1")
	if r := rehydrate(home1); r.Nap != nil {
		t.Fatalf("first boot restored nap %s", *r.Nap)
	}
	if body, _ := os.ReadFile(filepath.Join(home1, "needletail.yaml")); !strings.Contains(string(body), "generated:\n        - acme-knowledge\n        - acme-shared-docs") {
		t.Fatalf("needletail.yaml = %s", body)
	}
	// rehydrate's record of the booted skills is where the runtime looks: a repo skill is not
	// deleted at runtime (it would come back, and no deletion is proposed)
	if _, err := os.Stat(filepath.Join(home1, skills.BootedFile)); err != nil {
		t.Fatalf("booted.json: %v", err)
	}
	if _, err := (&skills.Library{Home: home1}).Delete("crm-api"); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Errorf("delete of a repo skill after rehydrate: %v", err)
	}

	// 2. The agent works: it writes a skill of its own (with a script), loads it, runs the script
	// from it, improves a repo skill, and keeps two memories. A generated skill is refused.
	fare := "---\nname: fare-lookup\ndescription: Look up a fare by route before quoting.\n---\n# Fare lookup\n\nRun `bash ${SKILL_DIR}/scripts/fare.sh <ORIGIN-DEST>` and quote the result in EUR.\n"
	model.script(
		toolStep("c1", "skill_manage", map[string]any{"action": "create", "name": "fare-lookup", "category": "crm", "content": fare}),
		toolStep("c2", "skill_manage", map[string]any{"action": "write_file", "name": "fare-lookup", "file_path": "scripts/fare.sh", "file_content": "echo \"fare $1: 420 EUR (from $SKILL_DIR)\"\n"}),
		toolStep("c3", "skill_view", map[string]any{"name": "fare-lookup"}),
		toolStep("c4", "terminal", map[string]any{"command": `bash "$SKILL_DIR/scripts/fare.sh" LIS-JFK`}),
		toolStep("c5", "skill_manage", map[string]any{"action": "patch", "name": "crm-api", "old_string": "Query Acme deals from the CRM API ($CRM_API_TOKEN).\n",
			"new_string": "Query Acme deals from the CRM API ($CRM_API_TOKEN).\n\nSort check-in sweeps with orderBy=segmentDate.\n"}),
		toolStep("c6", "skill_manage", map[string]any{"action": "patch", "name": "acme-knowledge", "old_string": "---", "new_string": "--- "}),
		toolStep("c7", "memory", map[string]any{"action": "add", "target": "memory", "content": "Fares come from the fare-lookup skill; Dana confirms them by phone at +1 415 555 0123."}),
		toolStep("c8", "memory", map[string]any{"action": "add", "target": "user", "content": "The owner wants fares quoted in EUR."}),
		textStep("Done."),
	)
	tools := oneshot(home1, "Quote the LIS-JFK fare and remember how.")
	if want := []string{"skill_manage", "skill_manage", "skill_view", "terminal", "skill_manage", "skill_manage", "memory", "memory"}; !slices.Equal(tools, want) {
		t.Errorf("tools = %v", tools)
	}
	results := strings.Join(model.seen, "\n")
	if !strings.Contains(results, `fare LIS-JFK: 420 EUR (from `+filepath.Join(home1, "skills", "crm", "fare-lookup")) {
		t.Errorf("the script did not run from its skill:\n%s", results)
	}
	if !strings.Contains(results, "generated by the build") {
		t.Errorf("a generated skill was not refused:\n%s", results)
	}
	if body, _ := os.ReadFile(filepath.Join(home1, "skills", "crm", "fare-lookup", "SKILL.md")); string(body) != fare {
		t.Fatalf("agent skill = %q", body)
	}

	// 3. Nap, then dream.
	t0 := time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC)
	n1 := nap(home1, "nt-1", t0)
	classes := map[string]string{}
	for _, f := range n1.Files {
		classes[f.Path] = string(f.Class)
	}
	for p, want := range map[string]string{"skills/crm/fare-lookup/SKILL.md": "learning", "skills/crm/fare-lookup/scripts/fare.sh": "learning",
		"skills/.usage.json": "learning", "memories/MEMORY.md": "learning", "memories/USER.md": "learning", "needletail.yaml": "learning"} {
		if classes[p] != want {
			t.Errorf("nap: %s is %q, want %s", p, classes[p], want)
		}
	}
	sessions := 0
	for p, c := range classes {
		if strings.Contains(p, ".tmp-") {
			t.Errorf("nap holds a temp file: %s", p)
		}
		if strings.HasPrefix(p, "sessions/") {
			sessions++
			if c != "raw" {
				t.Errorf("nap: %s is %s", p, c)
			}
		}
	}
	if sessions == 0 {
		t.Error("nap holds no session")
	}
	r1, reads := dream()
	if !slices.Equal(r1.Naps, []string{n1.ID}) || r1.NewLearnings != 2 || r1.Resighted != 0 {
		t.Errorf("dream = %+v", r1)
	}
	for _, k := range reads {
		if strings.Contains(k, "/raw/") {
			t.Errorf("the dream read raw history: %s", k)
		}
	}
	proposals := map[string]string{}
	for _, p := range r1.SkillProposals {
		proposals[p.Skill] = p.Status
	}
	if proposals["crm/fare-lookup"] != "new" || proposals["crm/crm-api"] != "changed" || len(proposals) != 2 {
		t.Errorf("skill proposals = %v", r1.SkillProposals)
	}
	prop := filepath.Join(learnings, "proposals", "skills", "crm", "fare-lookup")
	if body, _ := os.ReadFile(filepath.Join(prop, "SKILL.md")); string(body) != fare {
		t.Errorf("proposed SKILL.md = %q", body)
	}
	if body, _ := os.ReadFile(filepath.Join(prop, "scripts", "fare.sh")); !strings.Contains(string(body), "420 EUR") {
		t.Errorf("proposed script = %q", body)
	}
	var meta loop.SkillProposal
	readJSON(t, filepath.Join(prop, ".proposal.json"), &meta)
	if meta.Skill != "crm/fare-lookup" || meta.Nap != n1.ID || meta.NapHash == "" || meta.Stale {
		t.Errorf("proposal = %+v", meta)
	}
	ledger, err := learning.LoadLedger(root, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	var mem, user *learning.Learning
	for _, e := range ledger {
		switch {
		case e.Kind == learning.KindMemory && strings.Contains(e.Text, "fare-lookup"):
			mem = e
		case e.Kind == learning.KindUser && strings.Contains(e.Text, "EUR"):
			user = e
		}
	}
	if mem == nil || user == nil {
		t.Fatalf("ledger = %+v", ledger)
	}
	if strings.Contains(mem.Text, "555 0123") || !slices.Contains(mem.PII, "phone") || !slices.Contains(mem.Naps, n1.ID) || !slices.Contains(mem.Instances, "nt-1") {
		t.Errorf("memory sighting = %+v", mem)
	}
	var usage map[string]map[string]any
	readJSON(t, filepath.Join(learnings, "skill-usage.json"), &usage)
	if e := usage["fare-lookup"]; e["created_by"] != "agent" || e["view_count"] != float64(1) || e["use_count"] != float64(2) || e["patch_count"] != float64(1) {
		t.Errorf("fare-lookup usage = %v", e)
	}
	if e := usage["crm-api"]; e["patch_count"] != float64(1) || e["created_by"] != nil {
		t.Errorf("crm-api usage = %v", e)
	}
	if _, ok := usage["acme-knowledge"]; ok {
		t.Errorf("a refused change was counted: %v", usage["acme-knowledge"])
	}

	// 4. The next boot rehydrates from the nap: the agent's skill, its repo patch, its usage
	// counts and its memories come back, and it finds the skill again.
	home2 := filepath.Join(tmp, "home2")
	r := rehydrate(home2)
	if r.Nap == nil || *r.Nap != n1.ID || !slices.Contains(r.RestoredSkills, "crm/fare-lookup") || !slices.Contains(r.RestoredSkills, "crm/crm-api") {
		t.Fatalf("rehydrate = %+v", r)
	}
	if body, _ := os.ReadFile(filepath.Join(home2, "memories", "MEMORY.md")); !strings.Contains(string(body), "fare-lookup skill") {
		t.Errorf("MEMORY.md = %q", body)
	}
	model.script(
		toolStep("d1", "skill_view", map[string]any{"name": "crm/fare-lookup"}),
		toolStep("d2", "terminal", map[string]any{"command": "bash skills/crm/fare-lookup/scripts/fare.sh OPO-LHR"}),
		textStep("Done."),
	)
	if tools := oneshot(home2, "And OPO-LHR?"); !slices.Equal(tools, []string{"skill_view", "terminal"}) {
		t.Errorf("tools = %v", tools)
	}
	if results := strings.Join(model.seen, "\n"); !strings.Contains(results, "fare OPO-LHR: 420 EUR") || !strings.Contains(results, "quote the result in EUR") {
		t.Errorf("second boot:\n%s", results)
	}
	n2 := nap(home2, "nt-2", t0.Add(time.Hour))
	r2, _ := dream()
	if !slices.Equal(r2.Naps, []string{n2.ID}) || r2.NewLearnings != 0 || r2.Resighted != 2 {
		t.Errorf("second dream = %+v", r2)
	}
	readJSON(t, filepath.Join(learnings, "skill-usage.json"), &usage)
	if e := usage["fare-lookup"]; e["view_count"] != float64(2) || e["use_count"] != float64(4) || e["created_by"] != "agent" {
		t.Errorf("fare-lookup usage after the second boot = %v", e)
	}
	ledger, _ = learning.LoadLedger(root, "atlas")
	for _, e := range ledger {
		if strings.Contains(e.Text, "fare-lookup") && (e.SeenCount != 2 || !slices.Contains(e.Instances, "nt-2")) {
			t.Errorf("re-sighting = %+v", e)
		}
	}
	t.Logf("dream 1: %d new learnings, proposals %v; dream 2: %d re-sighted; fare-lookup usage %v",
		r1.NewLearnings, r1.SkillProposals, r2.Resighted, usage["fare-lookup"])
}
