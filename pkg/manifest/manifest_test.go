package manifest

import (
	"os"
	"os/exec"
	"path/filepath"
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
