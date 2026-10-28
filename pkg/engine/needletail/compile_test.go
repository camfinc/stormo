package needletail

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
var rebuilt = []string{"SOUL.md"}

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
	if strings.Join(c.Luau.Grants, ",") != "fs.read:home,fs.write:home/work/**,core:msg_*,http:api.example.com,fs.read:shared" {
		t.Errorf("grants = %v", c.Luau.Grants)
	}
	if len(c.Luau.HTTPSecrets) != 1 || c.Luau.HTTPSecrets[0].Env != "CRM_API_TOKEN" {
		t.Errorf("http_secrets = %+v", c.Luau.HTTPSecrets)
	}
	// memory.recall reaches needletail.yaml as written; unset, it is omitted (the runtime's
	// defaults), so the golden home has none
	if r := c.Memory.Recall; r == nil || r.Auto == nil || !*r.Auto || !r.Jobs || r.Max != 3 || r.Budget != 800 {
		t.Errorf("memory.recall = %+v", r)
	}
	if strings.Contains(string(raw), "recall:") != true {
		t.Errorf("needletail.yaml:\n%s", raw)
	}
	if golden, _ := os.ReadFile(filepath.Join("testdata", "home", "needletail.yaml")); strings.Contains(string(golden), "recall") {
		t.Error("an unset memory.recall was rendered")
	}
	for body, ok := range map[string]bool{"memory:\n  recall: {auto: false}\n": true, "memory:\n  recall: {max: 11}\n": false,
		"memory:\n  recall: {budget: 100}\n": false, "memory:\n  recall: {budget: 4000, max: 10}\n": true, "memory:\n  recall: {other: 1}\n": false} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readAgentConfig(dir); (err == nil) != ok {
			t.Errorf("%q: %v", body, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), body, 0o644); err != nil {
		t.Fatal(err)
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
	// A bad grant is refused; message grants name send_message targets.
	for g, ok := range map[string]bool{"exec:bash": false, "message:slack": true, "message:slack:*": true, "message:slack:C0DEVCHAN1:*": true,
		"message:slack:C0DEVCHAN1:1700000002.000100": true, "message:*": true, "message:slack:#general": false, "message:": false,
		"memory:read": true, "memory:write": true, "memory:*": false, "memory:hot": false,
		"followup:read": true, "followup:write": true, "followup:*": false, "follow_up:write": false} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("luau:\n  grants: [\""+g+"\"]\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readAgentConfig(dir); (err == nil) != ok {
			t.Errorf("grant %s: %v", g, err)
		}
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

// engine/needletail/config.yaml's timezone (the one Hermes keeps in engine/hermes/config.yaml)
// reaches needletail.yaml; a zone that does not exist is refused at build.
// luau.timeout (seconds) bounds the tools the model calls: limits.luau_timeout, apart from the
// schedules' script_timeout.
func TestLuauTimeoutRender(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("luau:\n  timeout: 45\n"), 0o644)
	ac, err := readAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	inst, a := atlas(t)
	a.Limits.ScriptTimeout = 900
	conn, _ := inst.Connection(a.Engine.Provider)
	c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac)
	if c.Limits.LuauTimeout != 45 || c.Limits.ScriptTimeout != 900 {
		t.Errorf("limits = %+v", c.Limits)
	}
	if c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), &agentConfig{}); c.Limits.LuauTimeout != 0 {
		t.Errorf("unset: %+v", c.Limits)
	}
	for _, bad := range []string{"-1", "601", "30s"} {
		_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("luau:\n  timeout: "+bad+"\n"), 0o644)
		if _, err := readAgentConfig(dir); err == nil {
			t.Errorf("luau.timeout %s accepted", bad)
		}
	}
}

// limits.background_lifetime (seconds) bounds the background processes: needletail.yaml's
// limits.background_lifetime, left out when unset.
func TestBackgroundLifetimeRender(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("limits:\n  background_lifetime: 3600\n"), 0o644)
	ac, err := readAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	if c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac); c.Limits.BackgroundLifetime != 3600 {
		t.Errorf("limits = %+v", c.Limits)
	}
	c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), &agentConfig{})
	if body, _ := yaml.Marshal(c); c.Limits.BackgroundLifetime != 0 || strings.Contains(string(body), "background_lifetime") {
		t.Errorf("unset: %+v", c.Limits)
	}
	for _, bad := range []string{"-1", "86401", "2h"} {
		_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("limits:\n  background_lifetime: "+bad+"\n"), 0o644)
		if _, err := readAgentConfig(dir); err == nil {
			t.Errorf("limits.background_lifetime %s accepted", bad)
		}
	}
}

func TestTimezoneRender(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("timezone: America/New_York\n"), 0o644)
	ac, err := readAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	raw, _ := yaml.Marshal(renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac))
	if !strings.Contains(string(raw), "\ntimezone: America/New_York\n") {
		t.Errorf("needletail.yaml:\n%s", raw)
	}
	raw, _ = yaml.Marshal(renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), &agentConfig{}))
	if strings.Contains(string(raw), "timezone") {
		t.Errorf("an unset timezone is rendered:\n%s", raw)
	}
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("timezone: Mars/Olympus\n"), 0o644)
	if _, err := readAgentConfig(dir); err == nil {
		t.Error("a bad timezone was accepted")
	}
}

// engine/needletail/config.yaml's slack.broadcast and schedules.turns, and the repo's job ids,
// reach needletail.yaml; none of them is rendered when unset.
func TestSlackAndSchedulesRender(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("slack:\n  broadcast: true\nschedules:\n  turns: 2\n"), 0o644)
	ac, err := readAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac)
	if c.Slack == nil || !c.Slack.Broadcast || c.Schedules.Turns != 2 {
		t.Errorf("slack %+v schedules %+v", c.Slack, c.Schedules)
	}
	if len(a.Schedules) == 0 || strings.Join(c.Schedules.Repo, ",") != a.Schedules[0].ID {
		t.Errorf("repo jobs %v, want the manifest's ids", c.Schedules.Repo)
	}
	a.Schedules = nil
	raw, _ := yaml.Marshal(renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), &agentConfig{}))
	if strings.Contains(string(raw), "slack:") || strings.Contains(string(raw), "schedules") {
		t.Errorf("unset settings rendered:\n%s", raw)
	}
	for _, bad := range []string{"schedules:\n  turns: -1\n", "schedules:\n  turns: 5\n", "slack:\n  broadcast: maybe\n"} {
		_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(bad), 0o644)
		if _, err := readAgentConfig(dir); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// engine/needletail/config.yaml's slack.listen (channel ids, or "*" alone for every channel the
// bot is in) reaches needletail.yaml; a DM, a name, "*" beside ids or a bare true is refused.
func TestSlackListenRender(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0o755)
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	for body, want := range map[string]string{"slack:\n  listen: ['*']\n": "*", "slack:\n  listen: [C0123456789, G0123456789]\n": "C0123456789,G0123456789"} {
		_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(body), 0o644)
		ac, err := readAgentConfig(dir)
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac)
		if c.Slack == nil || c.Slack.Broadcast || strings.Join(c.Slack.Listen, ",") != want {
			t.Errorf("%q: slack %+v", body, c.Slack)
		}
		raw, _ := yaml.Marshal(c)
		if !strings.Contains(string(raw), "listen:") || strings.Contains(string(raw), "broadcast") {
			t.Errorf("needletail.yaml:\n%s", raw)
		}
	}
	for _, bad := range []string{"slack:\n  listen: true\n", "slack:\n  listen: [general]\n", "slack:\n  listen: [D0123456789]\n",
		"slack:\n  listen: ['*', C0123456789]\n", "slack:\n  listen: [c0123456789]\n"} {
		_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(bad), 0o644)
		if _, err := readAgentConfig(dir); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestBackgroundLearningConfigOptIn(t *testing.T) {
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0755)
	for _, enabled := range []bool{false, true} {
		body := "background_learning: false\n"
		if enabled {
			body = "background_learning: true\n"
		}
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		ac, err := readAgentConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac)
		if c.BackgroundLearning != enabled || c.Reflection {
			t.Fatalf("config=%+v", c)
		}
	}
}

func TestSkillMaintenanceIndependentOptIn(t *testing.T) {
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "skill_maintenance: true\n"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		ac, err := readAgentConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac)
		if c.SkillMaintenance != (body != "") || c.BackgroundLearning || c.Reflection {
			t.Fatalf("unexpected independent flags: %+v", c)
		}
	}
}

// engine/needletail/config.yaml's approvals and slack.relay_bots reach needletail.yaml; bad ids,
// bounds and patterns are refused; with neither, nothing is rendered (older images keep working).
func TestApprovalsRender(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0o755)
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	body := "approvals:\n  approvers: [U0OWNER001]\n  timeout: 900\n  delivery_expiry_days: 2\n  per_conversation: 2\n  per_day: 10\n  commands:\n    - {name: prod db, pattern: 'psql .*prod'}\nslack:\n  relay_bots: [B0FORMBOT1]\n"
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(body), 0o644)
	ac, err := readAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac)
	raw, _ := yaml.Marshal(c)
	for _, want := range []string{"approvers:", "U0OWNER001", "timeout: 900", "delivery_expiry_days: 2", "per_conversation: 2", "per_day: 10", "psql .*prod", "relay_bots:", "B0FORMBOT1"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("needletail.yaml lacks %q:\n%s", want, raw)
		}
	}
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("timezone: UTC\n"), 0o644)
	ac, _ = readAgentConfig(dir)
	raw, _ = yaml.Marshal(renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac))
	if strings.Contains(string(raw), "approvals") || strings.Contains(string(raw), "relay_bots") {
		t.Errorf("rendered without config:\n%s", raw)
	}
	for _, bad := range []string{"approvals:\n  approvers: [owner]\n", "approvals:\n  approvers: [B0BOT00001]\n", "approvals:\n  timeout: 10\n",
		"approvals:\n  per_day: 1000\n", "approvals:\n  delivery_expiry_days: 60\n", "approvals:\n  commands: [{pattern: '('}]\n",
		"approvals:\n  unknown: 1\n", "slack:\n  relay_bots: [formbot]\n"} {
		_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(bad), 0o644)
		if _, err := readAgentConfig(dir); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// engine/needletail/config.yaml's files and stt reach needletail.yaml; bad values are refused; with
// neither, nothing is rendered (older images keep working); stt needs its key declared.
func TestFilesAndSTTRender(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0o755)
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	body := "files:\n  retention_days: 7\n  vision: false\nstt:\n  enabled: true\n  provider: elevenlabs\n  language: es\n"
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(body), 0o644)
	ac, err := readAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac)
	raw, _ := yaml.Marshal(c)
	for _, want := range []string{"retention_days: 7", "vision: \"off\"", "enabled: true", "provider: elevenlabs", "language: es"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("needletail.yaml lacks %q:\n%s", want, raw)
		}
	}
	if err := checkSTTKey(a, ac); err != nil { // the example declares it
		t.Error(err)
	}
	declared := a.Secrets
	a.Secrets = slices.DeleteFunc(slices.Clone(a.Secrets), func(s string) bool { return s == "ELEVENLABS_API_KEY" })
	if err := checkSTTKey(a, ac); err == nil {
		t.Error("stt enabled without ELEVENLABS_API_KEY among the secrets")
	}
	a.Secrets = declared
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("timezone: UTC\n"), 0o644)
	ac, _ = readAgentConfig(dir)
	raw, _ = yaml.Marshal(renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac))
	if strings.Contains(string(raw), "files:") || strings.Contains(string(raw), "stt:") {
		t.Errorf("rendered without config:\n%s", raw)
	}
	for _, bad := range []string{"files:\n  retention_days: 400\n", "files:\n  retention_days: -1\n", "files:\n  vision: maybe\n",
		"stt:\n  enabled: true\n  provider: whisper\n", "stt:\n  language: Spanish\n", "stt:\n  model: 'a b'\n", "stt:\n  unknown: 1\n"} {
		_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte(bad), 0o644)
		if _, err := readAgentConfig(dir); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// limits.chat_turns is how many chat turns (the API's and Slack's) may run at once:
// needletail.yaml's limits.chat_turns, left out when unset (the runtime's 2), 1 to 16.
func TestChatTurnsRender(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "engine", "needletail"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("limits:\n  chat_turns: 4\n"), 0o644)
	ac, err := readAgentConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	inst, a := atlas(t)
	conn, _ := inst.Connection(a.Engine.Provider)
	if c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), ac); c.Limits.ChatTurns != 4 {
		t.Errorf("limits = %+v", c.Limits)
	}
	c := renderConfig(a, conn, manifest.AWS, manifest.CoreFor(inst, manifest.AWS), &agentConfig{})
	if body, _ := yaml.Marshal(c); c.Limits.ChatTurns != 0 || strings.Contains(string(body), "chat_turns") {
		t.Errorf("unset: %+v", c.Limits)
	}
	for _, bad := range []string{"-1", "17", "two"} {
		_ = os.WriteFile(filepath.Join(dir, filepath.FromSlash(ConfigFile)), []byte("limits:\n  chat_turns: "+bad+"\n"), 0o644)
		if _, err := readAgentConfig(dir); err == nil {
			t.Errorf("limits.chat_turns %s accepted", bad)
		}
	}
}
