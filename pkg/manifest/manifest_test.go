package manifest

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

const example = "../../examples/minimal"

func atlas(t *testing.T) *Agent {
	t.Helper()
	a, err := Load(example, "atlas", "acme/stormo")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

var required = []string{"OPENROUTER_API_KEY", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "APIFY_API_TOKEN", "ELEVENLABS_API_KEY", "API_SERVER_KEY"}
var gated = []string{"crm/crm-api", "crm/crm-documents-api", "crm/crm-quote-api", "crm/hourly_deal_monitor"}

func TestDefaultsAndOrder(t *testing.T) {
	a := atlas(t)
	if a.Deploy.SecretID != "acme/stormo/atlas" || a.Learning.SeedFill != 0.6 || a.Deploy.Memory != 3072 || a.Engine.Provider != "openrouter" {
		t.Errorf("%+v %+v", a.Deploy, a.Learning)
	}
	scout, _ := Load(example, "scout", "acme/stormo")
	if v, _ := scout.Env.Get("BOT_MODE"); v != "dry-run" {
		t.Errorf("env = %v", scout.Env.Map())
	}
	if ids := AgentIDs(example); !slices.Equal(ids, []string{"atlas", "nova", "scout"}) {
		t.Errorf("ids = %v", ids)
	}
}

func TestAbsentOptionalSecretsSwitchOffWhatTheyGate(t *testing.T) {
	a := atlas(t)
	eff := ApplyOptional(a, required)
	if len(eff.Agent.Channels) != 1 || eff.Agent.Channels[0].Kind != "slack" || len(eff.Agent.Actions) != 0 || slices.Contains(eff.Agent.Secrets, "CRM_API_TOKEN") {
		t.Errorf("effective = %+v", eff.Agent)
	}
	skip := append([]string{}, eff.SkipSkills...)
	sort.Strings(skip)
	if !slices.Equal(skip, gated) {
		t.Errorf("skip = %v", skip)
	}
	for _, o := range eff.Off {
		if o.Name == "TELEGRAM_BOT_TOKEN" && !slices.Equal(o.What, []string{"telegram channel"}) {
			t.Errorf("telegram off = %v", o.What)
		}
	}
	full := ApplyOptional(a, append(append([]string{}, required...), "CRM_API_TOKEN", "TELEGRAM_BOT_TOKEN"))
	if len(full.Agent.Channels) != 2 || !slices.Equal(full.Agent.Actions, []string{"sales.crm.deals.list"}) || len(full.SkipSkills) != 0 {
		t.Errorf("full = %+v", full)
	}
}

func copyInstance(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"units", "agents"} {
		if err := exec.Command("cp", "-R", filepath.Join(example, d), filepath.Join(root, d)).Run(); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestManifestRejections(t *testing.T) {
	root := copyInstance(t)
	yamlPath := filepath.Join(root, "agents", "atlas", "agent.yaml")
	original, _ := os.ReadFile(yamlPath)
	for _, c := range []struct {
		edit func(string) string
		err  string
	}{
		{func(s string) string {
			return strings.Replace(s, "  - APIFY_API_TOKEN", "  - APIFY_API_TOKEN\n  - CRM_API_TOKEN", 1)
		}, "both secrets: and optional_secrets:"},
		{func(s string) string {
			return strings.Replace(s, "      - crm/crm-api", "      - crm/no-such-skill", 1)
		}, "which is not in agents/atlas/skills/"},
		{func(s string) string {
			return strings.Replace(s, "    # allowed_users: [123456789]", "    allowed_users: [U0ABCDEFG]", 1)
		}, "not a numeric user id"},
		{func(s string) string {
			return strings.Replace(s, "allowed_users: [U0000000001]", "allowed_users: [ann]", 1)
		}, "is not a Member ID"},
		{func(s string) string { return strings.Replace(s, "unit: sales", "unit: nowhere", 1) }, `unknown unit "nowhere"`},
		{func(s string) string {
			return strings.Replace(s, "  - sales.crm.deals.list\n\nstate", "  - support.x.y.z\n\nstate", 1)
		}, "belongs to unit"},
	} {
		os.WriteFile(yamlPath, []byte(c.edit(string(original))), 0o644)
		_, err := Load(root, "atlas", "acme/stormo")
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("want %q, got %v", c.err, err)
		}
		if _, ok := err.(*Error); err != nil && !ok {
			t.Errorf("%v is not a manifest.Error", err)
		}
	}
}

func TestSlackNeedsBothTokens(t *testing.T) {
	root := copyInstance(t)
	dir := filepath.Join(root, "agents", "x")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SOUL.md"), []byte("x"), 0o644)
	write := func(extra string) {
		os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("id: x\nname: X\nunit: sales\nengine: {kind: hermes, version: 1, model: m}\n"+extra), 0o644)
	}
	write("channels: [{kind: slack}]\nsecrets: [SLACK_BOT_TOKEN]\n")
	if _, err := Load(root, "x", "p"); err == nil || !regexp.MustCompile(`needs secret SLACK_APP_TOKEN`).MatchString(err.Error()) {
		t.Errorf("err = %v", err)
	}
	write("channels: [{kind: slack, allowed_users: [U01ABC2DEF3]}]\nsecrets: [SLACK_BOT_TOKEN, SLACK_APP_TOKEN]\n")
	if a, err := Load(root, "x", "p"); err != nil || a.Channels[0].Kind != "slack" {
		t.Errorf("valid manifest rejected: %v", err)
	}
}

// parseAs is atlas's manifest with its body replaced: the instance and SOUL.md stay the example's.
func parseAs(t *testing.T, body string) (*Agent, error) {
	t.Helper()
	return Parse(example, "atlas", "acme/stormo", []byte(body))
}

const atlasHead = "id: atlas\nname: Atlas\nunit: sales\n"

func TestFormatOneReadsLikeFormatZero(t *testing.T) {
	legacy, err := parseAs(t, atlasHead+"engine:\n  kind: hermes\n  version: 0.21.5\n  model: openai/gpt-6-luna\n  provider: openrouter\n  local: {via: core, model: gpt-5.6-luna}\nlearning:\n  memory_char_limit: 3000\n  user_char_limit: 1000\n  seed_fill: 0.5\n")
	if err != nil {
		t.Fatal(err)
	}
	now, err := parseAs(t, atlasHead+"format: 1\nengine:\n  kind: hermes\n  version: 0.21.5\nmodel:\n  name: openai/gpt-6-luna\n  provider: openrouter\n  local: {via: core, name: gpt-5.6-luna}\nmemory:\n  agent: 3000\n  user: 1000\nlearning:\n  seed_fill: 0.5\nlimits:\n  turns: 40\n  reasoning: max\n  command_timeout: 600\n  script_timeout: 900\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(legacy.Engine, now.Engine) || legacy.Learning != now.Learning {
		t.Errorf("engine %+v vs %+v, learning %+v vs %+v", legacy.Engine, now.Engine, legacy.Learning, now.Learning)
	}
	if legacy.Format != 0 || !slices.Equal(legacy.Legacy, []string{"engine.model", "engine.provider", "engine.local", "learning.memory_char_limit", "learning.user_char_limit"}) {
		t.Errorf("legacy %d %v", legacy.Format, legacy.Legacy)
	}
	if now.Format != 1 || len(now.Legacy) != 0 || now.Limits != (Limits{Turns: 40, Reasoning: "max", CommandTimeout: 600, ScriptTimeout: 900}) {
		t.Errorf("format 1: %d %v %+v", now.Format, now.Legacy, now.Limits)
	}
}

func TestFormatRefusals(t *testing.T) {
	eng := "engine:\n  kind: hermes\n  version: 0.21.5\n"
	for body, want := range map[string]string{
		atlasHead + "format: 1\n" + eng + "  model: x\n":                                              "format 1 has no engine.model",
		atlasHead + "format: 2\n" + eng + "model: {name: x}\n":                                        "newer than this stormo",
		atlasHead + eng + "  model: x\nmodel: {name: y}\n":                                            "both set",
		atlasHead + eng + "model: {name: x}\nmemory: {agent: 10}\nlearning: {memory_char_limit: 9}\n": "same budget",
		atlasHead + eng + "model: {name: x}\nlimits: {turns: -1}\n":                                   "positive whole number",
		atlasHead + eng + "model: {name: x}\nlimits: {speed: 3}\n":                                    "unknown limit",
		atlasHead + eng: "model.name are required",
	} {
		if _, err := parseAs(t, body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", body, want, err)
		}
	}
}

func TestSchedules(t *testing.T) {
	base := atlasHead + "engine:\n  kind: hermes\n  version: 0.21.5\nmodel: {name: x}\nschedules:\n"
	a, err := parseAs(t, base+"  - id: digest\n    name: Digest\n    every: 2h\n    prompt: Post the digest\n    skills: [crm/crm-api]\n  - id: weekly\n    cron: \"0 9 * * 1\"\n    prompt: Plan\n    enabled: false\n    note: later\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Schedules) != 2 || a.Schedules[0].Minutes() != 120 || !a.Schedules[0].Agent || !a.Schedules[0].Enabled || a.Schedules[1].Enabled || a.Schedules[1].Minutes() != 0 {
		t.Errorf("schedules = %+v", a.Schedules)
	}
	if none, _ := parseAs(t, atlasHead+"engine:\n  kind: hermes\n  version: 0.21.5\nmodel: {name: x}\n"); none.Schedules != nil {
		t.Error("no schedules: must read as nil (the engine's own file, format 0)")
	}
	for body, want := range map[string]string{
		"  - {id: a, prompt: p}\n":                                               "exactly one of every",
		"  - {id: a, every: 2h, cron: \"* * * * *\", prompt: p}\n":               "exactly one of every",
		"  - {id: a, every: 90s, prompt: p}\n":                                   "every is a number",
		"  - {id: a, cron: \"* *\", prompt: p}\n":                                "five fields",
		"  - {id: a, every: 1h}\n":                                               "a prompt or a script",
		"  - {id: a, every: 1h, agent: false, prompt: p}\n":                      "runs only a script",
		"  - {id: a, every: 1h, prompt: p}\n  - {id: a, every: 2h, prompt: q}\n": "used twice",
		"  - {id: a, every: 1h, script: nope.py}\n":                              "not in agents/atlas/scripts",
		"  - {id: a, every: 1h, prompt: p, when: now}\n":                         "unknown key",
		"  - {every: 1h, prompt: p}\n":                                           "id is required",
	} {
		if _, err := parseAs(t, base+body); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", body, want, err)
		}
	}
}
