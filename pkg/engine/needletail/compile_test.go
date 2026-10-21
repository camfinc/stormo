package needletail

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/manifest"
	"go.yaml.in/yaml/v3"
)

var update = flag.Bool("update", false, "rewrite testdata/home from the current Compile")

// rebuilt are baseline files no snapshot rule claims: rehydrate rebuilds them from the baseline.
var rebuilt = []string{"SOUL.md", "scripts/**"}

func TestCompileAtlasGolden(t *testing.T) {
	inst, a := atlas(t)
	n := New(inst)
	// The context `stormo build` hands the engine (knowledge skills, seed memory), so the golden
	// home is what a build of atlas with engine.kind: needletail produces.
	ctx, err := learning.CompileLearning(inst, a.ID, n.Memory())
	if err != nil {
		t.Fatal(err)
	}
	ctx.Target = manifest.AWS
	files, err := n.Compile(a, ctx)
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "home")
	if *update {
		_ = os.RemoveAll(golden)
		for p, body := range files {
			dst := filepath.Join(golden, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := map[string]string{}
	err = filepath.WalkDir(golden, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(p)
		rel, _ := filepath.Rel(golden, p)
		want[filepath.ToSlash(rel)] = string(body)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for p := range files {
		got = append(got, p)
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Errorf("compiled %d files, golden has %d: %v (go test -run Golden -update rewrites it)", len(got), len(want), got)
	}
	for _, p := range got {
		if w, ok := want[p]; !ok {
			t.Errorf("%s: not in the golden home", p)
		} else if w != string(files[p]) {
			t.Errorf("%s differs from the golden home:\n%s", p, files[p])
		}
		if engine.Classify(p, n.Layout().Rules) == nil && !isRebuilt(p) {
			t.Errorf("%s: emitted but neither classified nor rebuilt from the baseline", p)
		}
	}
	for _, p := range []string{"needletail.yaml", "cron/jobs.json", "SOUL.md", "memories/MEMORY.md", "skills/crm/crm-api/SKILL.md"} {
		if _, ok := files[p]; !ok {
			t.Errorf("%s missing", p)
		}
	}
}

func isRebuilt(p string) bool {
	for _, g := range rebuilt {
		if engine.Match(g, p) {
			return true
		}
	}
	return false
}

func TestCompileRefusesUnknownReasoning(t *testing.T) {
	inst, a := atlas(t)
	a.Limits.Reasoning = "galaxy-brain"
	if _, err := New(inst).Compile(a, engine.CompileContext{Instance: inst, Target: manifest.AWS}); err == nil || !strings.Contains(err.Error(), "reasoning") {
		t.Errorf("err = %v", err)
	}
}

func TestGrantsRender(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile("testdata/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), body, 0o644); err != nil {
		t.Fatal(err)
	}
	ac, err := readAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	core := manifest.CoreFor(inst, manifest.Local)
	raw, err := yaml.Marshal(renderConfig(a, conn, manifest.Local, core, ac))
	if err != nil {
		t.Fatal(err)
	}
	var c wire.Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Luau.Grants, ",") != "fs.read:home,fs.write:home,core:msg_*,http:api.example.com,fs.read:shared" {
		t.Errorf("grants = %v", c.Luau.Grants)
	}
	if len(c.Luau.HTTPSecrets) != 1 || c.Luau.HTTPSecrets[0].Env != "CRM_API_TOKEN" {
		t.Errorf("http_secrets = %+v", c.Luau.HTTPSecrets)
	}
	// On the core: its gateway first (the instance's own core), the agent's connection as the
	// fallback; hooks and MCP through the sidecar's relay.
	if c.Model.Primary.BaseURL != core.BaseURL || len(c.Model.Fallbacks) != 1 || c.Model.Fallbacks[0].KeyEnv != conn.Key || c.Core == nil ||
		c.Core.MCPURL != manifest.RelayURL(manifest.Local)+"/mcp" || c.Core.IngestURL != manifest.RelayURL(manifest.Local)+"/ingest" {
		t.Errorf("model = %+v core = %+v", c.Model, c.Core)
	}
	// Another instance's core port is the one rendered.
	inst.CorePortSetting = 28600
	other := renderConfig(a, conn, manifest.Local, manifest.CoreFor(inst, manifest.Local), ac)
	if !strings.Contains(other.Model.Primary.BaseURL, ":28600/") {
		t.Errorf("core port: %s", other.Model.Primary.BaseURL)
	}
	// A placement without the ChatGPT gateway keeps the connection as its model, core tools still on.
	noGW := core
	noGW.NoGateway = true
	if c := renderConfig(a, conn, manifest.Local, noGW, ac); c.Model.Primary != (wire.Endpoint{BaseURL: conn.BaseURL, KeyEnv: conn.Key, Model: a.Engine.Model}) || c.Core == nil {
		t.Errorf("no gateway: %+v", c)
	}
	// A bad grant is refused.
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("luau:\n  grants: [exec:bash]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readAgentConfig(dir); err == nil {
		t.Error("exec:bash granted")
	}
}

// The build's generated skills reach the runtime (needletail.yaml skills.generated), which never
// lets skill_manage change them.
func TestGeneratedSkills(t *testing.T) {
	got := generatedSkills(map[string]string{
		"acme-knowledge/SKILL.md":            "x",
		"acme-knowledge/references/agent.md": "x",
		"acme-shared-docs/SKILL.md":          "x",
		"acme-shared-docs/scripts/s.py":      "x",
		"loose/notes.md":                     "x",
	})
	if strings.Join(got, ",") != "acme-knowledge,acme-shared-docs,loose" {
		t.Errorf("generated = %v", got)
	}
	if got := generatedSkills(nil); len(got) != 0 {
		t.Errorf("none = %v", got)
	}
	var cfg wire.Config
	body, err := os.ReadFile(filepath.Join("testdata", "home", "needletail.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(body, &cfg); err != nil || strings.Join(cfg.Skills.Generated, ",") != "acme-knowledge,acme-shared-docs" {
		t.Errorf("golden skills = %+v %v", cfg.Skills, err)
	}
}
