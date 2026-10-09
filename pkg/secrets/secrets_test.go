package secrets

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/aws"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

const file = `
shared:
  OPENROUTER_API_KEY: or-shared
  UNUSED_GLOBAL: nope
units:
  sales:
    CRM_API_TOKEN: crm-sales
  support:
    CRM_API_TOKEN: wrong-unit
agents:
  atlas:
    OPENROUTER_API_KEY: or-atlas
    SLACK_BOT_TOKEN: xoxb-prod
    SLACK_APP_TOKEN: xapp-prod
    APIFY_API_TOKEN: apify
    ELEVENLABS_API_KEY: ""
    API_SERVER_KEY: k
local:
  agents:
    atlas:
      SLACK_BOT_TOKEN: xoxb-dev
      SLACK_APP_TOKEN: xapp-dev
      ELEVENLABS_API_KEY: el-local
      SWARM_CORE_KEY: core
`

func setup(t *testing.T) (*File, *manifest.Agent) {
	t.Helper()
	f, err := Parse([]byte(file))
	if err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return f, a
}

func val(r Resolved, k string) string { v, _ := r.Values.Get(k); return v }

func TestResolveLayersAndDeclaredNamesOnly(t *testing.T) {
	f, a := setup(t)
	r := Resolve(f, a, manifest.AWS)
	if val(r, "OPENROUTER_API_KEY") != "or-atlas" || val(r, "CRM_API_TOKEN") != "crm-sales" || val(r, "SLACK_APP_TOKEN") != "xapp-prod" {
		t.Errorf("values = %v", r.Values.Map())
	}
	if _, ok := r.Values.Get("UNUSED_GLOBAL"); ok {
		t.Error("undeclared name leaked")
	}
	if !slices.Equal(r.Missing, []string{"ELEVENLABS_API_KEY"}) {
		t.Errorf("missing = %v (empty counts as missing)", r.Missing)
	}
}

func TestLocalOverlayAppliesOnlyLocally(t *testing.T) {
	f, a := setup(t)
	r := Resolve(f, a, manifest.Local)
	if val(r, "SLACK_APP_TOKEN") != "xapp-dev" || val(r, "ELEVENLABS_API_KEY") != "el-local" || len(r.Missing) != 0 {
		t.Errorf("local: %v missing %v", r.Values.Map(), r.Missing)
	}
	got := append([]string{}, r.Overridden...)
	sort.Strings(got)
	if !slices.Equal(got, []string{"ELEVENLABS_API_KEY", "SLACK_APP_TOKEN", "SLACK_BOT_TOKEN", "SWARM_CORE_KEY"}) {
		t.Errorf("overridden = %v", got)
	}
	if _, ok := Resolve(f, a, manifest.AWS).Values.Get("SWARM_CORE_KEY"); ok {
		t.Error("the core key is local-only and must not resolve for AWS")
	}
}

func TestProdTokenClashes(t *testing.T) {
	f, a := setup(t)
	if c := ProdTokenClashes(f, a); len(c) != 0 {
		t.Errorf("clashes with dev tokens: %v", c)
	}
	f.Local = nil
	if c := ProdTokenClashes(f, a); !slices.Equal(c, []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN"}) {
		t.Errorf("no overlay: %v", c)
	}
	// Socket Mode splits events across connections: a dev bot token on the prod app token still clashes.
	f2, _ := Parse([]byte(file + "\n"))
	f2.Local.Agents.Get("atlas").Delete("SLACK_APP_TOKEN")
	if c := ProdTokenClashes(f2, a); !slices.Equal(c, []string{"SLACK_APP_TOKEN"}) {
		t.Errorf("half overlay: %v", c)
	}
}

func TestEnvFileRejectsMultiLineValues(t *testing.T) {
	f, a := setup(t)
	body, err := RenderEnvFile(Resolve(f, a, manifest.Local).Values)
	if err != nil || !strings.Contains(body, "SLACK_BOT_TOKEN=xoxb-dev\n") {
		t.Errorf("%q %v", body, err)
	}
	r := Resolve(f, a, manifest.Local)
	r.Values.Set("BAD", "a\nb")
	if _, err := RenderEnvFile(r.Values); err == nil {
		t.Error("multi-line value accepted")
	}
}

// Init and Share rewrite the owner's real secrets file: values, layer order and 0600 must survive.
func TestInitKeepsValuesMintsKeysAndIsPrivate(t *testing.T) {
	root := t.TempDir()
	ex := "../../examples/minimal"
	for _, d := range []string{"units", "agents"} {
		if err := exec.Command("cp", "-R", filepath.Join(ex, d), filepath.Join(root, d)).Run(); err != nil {
			t.Fatal(err)
		}
	}
	body, _ := os.ReadFile(filepath.Join(ex, "stormo.yaml"))
	os.WriteFile(filepath.Join(root, "stormo.yaml"), body, 0o644)
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, FileName)
	os.WriteFile(path, []byte("shared:\n  OPENROUTER_API_KEY: keep-me\nagents:\n  atlas:\n    APIFY_API_TOKEN: \"0123\"\n"), 0o644)
	_, added, err := Init(inst)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(added, "atlas.OPENROUTER_API_KEY") || !slices.Contains(added, "atlas.ELEVENLABS_API_KEY") {
		t.Errorf("added = %v", added)
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Shared.Get("OPENROUTER_API_KEY"); v != "keep-me" {
		t.Errorf("shared value lost: %q", v)
	}
	if v, _ := f.Agents.Get("atlas").Get("APIFY_API_TOKEN"); v != "0123" {
		t.Errorf("quoted value changed: %q", v)
	}
	key := regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`)
	if v, _ := f.Agents.Get("atlas").Get("API_SERVER_KEY"); !key.MatchString(v) {
		t.Errorf("API_SERVER_KEY not minted: %q", v)
	}
	// The core key is minted per agent under local: only, so push never sends it to AWS.
	if v, _ := f.Local.Agents.Get("atlas").Get("SWARM_CORE_KEY"); !key.MatchString(v) {
		t.Errorf("SWARM_CORE_KEY not minted locally: %q", v)
	}
	if _, ok := f.Agents.Get("atlas").Get("SWARM_CORE_KEY"); ok {
		t.Error("core key written to the aws layer")
	}
	info, _ := os.Stat(path)
	raw, _ := os.ReadFile(path)
	if info.Mode().Perm() != 0o600 || !strings.HasPrefix(string(raw), "# acme-stormo secrets. NEVER COMMIT") {
		t.Errorf("mode %v, header %q", info.Mode().Perm(), strings.SplitN(string(raw), "\n", 2)[0])
	}
	// A format-1 agent's own values live in its folder, not in secrets.local.yaml.
	own, err := os.ReadFile(AgentFile(root, "atlas"))
	if strings.Contains(string(raw), "\nagents:") || err != nil || !strings.Contains(string(own), `APIFY_API_TOKEN: "0123"`) {
		t.Errorf("secrets.local.yaml:\n%s\natlas's own file:\n%s", raw, own)
	}
	if info, _ := os.Stat(AgentFile(root, "atlas")); info.Mode().Perm() != 0o600 {
		t.Errorf("atlas's own file mode %v", info.Mode().Perm())
	}
	if _, again, _ := Init(inst); len(again) != 0 {
		t.Errorf("second init added %v", again)
	}
}

func TestFileProblems(t *testing.T) {
	root := t.TempDir()
	exec.Command("git", "init", "-q", root).Run()
	path := filepath.Join(root, FileName)
	os.WriteFile(path, []byte("shared: {}\n"), 0o644)
	got := FileProblems(root)
	if !slices.Equal(got, []string{path + " is not gitignored", path + " is readable by group/others: chmod 600 it"}) {
		t.Errorf("problems = %v", got)
	}
	cmd := exec.Command("git", "add", "-f", FileName)
	cmd.Dir = root
	cmd.Run()
	if p := FileProblems(root); len(p) == 0 || !strings.Contains(p[0], "is tracked by git") {
		t.Errorf("tracked: %v", p)
	}
}

func fakeAWS(existing string) (aws.CLI, *[][]string, *[]string) {
	calls, stdins := &[][]string{}, &[]string{}
	return func(args []string, stdin *string) aws.Result {
		*calls = append(*calls, args)
		if stdin != nil {
			*stdins = append(*stdins, *stdin)
		}
		if args[1] == "get-secret-value" {
			if existing == "" {
				return aws.Result{Code: 254, Stderr: "An error occurred (ResourceNotFoundException)"}
			}
			return aws.Result{Stdout: existing}
		}
		return aws.Result{Stdout: "{}"}
	}, calls, stdins
}

func TestPushPlansByKeyNameAndSendsValuesOnStdinOnly(t *testing.T) {
	f, a := setup(t)
	r := Resolve(f, a, manifest.Local)
	cli, calls, stdins := fakeAWS(`{"OPENROUTER_API_KEY":"or-atlas","SLACK_BOT_TOKEN":"old","STALE":"x"}`)
	plan, err := PlanPush(a, r, "us-east-2", cli)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Exists || !slices.Equal(plan.Unchanged, []string{"OPENROUTER_API_KEY"}) || !slices.Contains(plan.Changed, "SLACK_BOT_TOKEN") || !slices.Equal(plan.Removed, []string{"STALE"}) || !slices.Contains(plan.Added, "CRM_API_TOKEN") {
		t.Errorf("plan = %+v", plan)
	}
	if err := ApplyPush(plan, r, "us-east-2", "acme-stormo", cli); err != nil {
		t.Fatal(err)
	}
	put := (*calls)[len(*calls)-1]
	if put[1] != "put-secret-value" || !slices.Contains(put, "file:///dev/stdin") || strings.Contains(strings.Join(put, " "), "crm-sales") {
		t.Errorf("put = %v", put)
	}
	if !strings.Contains((*stdins)[0], `"CRM_API_TOKEN":"crm-sales"`) {
		t.Errorf("stdin = %s", (*stdins)[0])
	}
}

func TestPushCreatesAMissingSecret(t *testing.T) {
	f, a := setup(t)
	r := Resolve(f, a, manifest.Local)
	cli, calls, _ := fakeAWS("")
	plan, err := PlanPush(a, r, "us-east-2", cli)
	if err != nil || plan.Exists {
		t.Fatalf("%+v %v", plan, err)
	}
	ApplyPush(plan, r, "us-east-2", "acme-stormo", cli)
	last := (*calls)[len(*calls)-1]
	if !slices.Equal(last[:5], []string{"secretsmanager", "create-secret", "--region", "us-east-2", "--name"}) || !slices.Contains(last, "acme/stormo/atlas") {
		t.Errorf("create = %v", last)
	}
}

func TestShare(t *testing.T) {
	f, err := Parse([]byte(`
agents:
  atlas: {OPENROUTER_API_KEY: or-1, APIFY_API_TOKEN: ap-1, SLACK_BOT_TOKEN: xoxb-m}
  scout: {OPENROUTER_API_KEY: or-1}
  nova: {OPENROUTER_API_KEY: "", APIFY_API_TOKEN: ap-other}
`))
	if err != nil {
		t.Fatal(err)
	}
	r, err := Share(f, []string{"OPENROUTER_API_KEY", "APIFY_API_TOKEN", "ELEVENLABS_API_KEY"}, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Shared.Get("APIFY_API_TOKEN"); v != "ap-1" {
		t.Errorf("shared = %v", f.Shared.Map())
	}
	removed := append([]string{}, r.Removed...)
	sort.Strings(removed)
	if !slices.Equal(removed, []string{"atlas.APIFY_API_TOKEN", "atlas.OPENROUTER_API_KEY", "nova.OPENROUTER_API_KEY", "scout.OPENROUTER_API_KEY"}) || !slices.Equal(r.Kept, []string{"nova.APIFY_API_TOKEN"}) || !slices.Equal(r.Missing, []string{"ELEVENLABS_API_KEY"}) {
		t.Errorf("result = %+v", r)
	}
	if v, _ := f.Agents.Get("atlas").Get("SLACK_BOT_TOKEN"); v != "xoxb-m" {
		t.Error("per-agent token touched")
	}
	if _, err := Share(f, []string{"SLACK_BOT_TOKEN"}, "atlas"); err == nil || !strings.Contains(err.Error(), "one agent by design") {
		t.Errorf("err = %v", err)
	}
}

func TestAgentSecretsLiveInTheAgentsFolder(t *testing.T) {
	root := t.TempDir()
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(Path(root), "shared:\n  OPENROUTER_API_KEY: or\nagents:\n  nova:\n    SLACK_BOT_TOKEN: nova-bot\n")
	write(AgentFile(root, "atlas"), "secrets:\n  SLACK_BOT_TOKEN: atlas-bot\nlocal:\n  SWARM_CORE_KEY: atlas-core\n")
	before := Stamp(root)
	f, err := Load(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Agents.Get("atlas").Get("SLACK_BOT_TOKEN"); v != "atlas-bot" || !f.InFolder("atlas") || f.InFolder("nova") {
		t.Fatalf("atlas = %q, in folder %v", v, f.InFolder("atlas"))
	}
	if v, _ := f.Local.Agents.Get("atlas").Get("SWARM_CORE_KEY"); v != "atlas-core" {
		t.Errorf("local = %q", v)
	}
	// Save keeps each agent's layers where they live.
	f.Agents.Get("atlas").Set("API_SERVER_KEY", "k")
	f.MoveToFolder("nova")
	if err := Save(Path(root), "acme-stormo", f); err != nil {
		t.Fatal(err)
	}
	main, _ := os.ReadFile(Path(root))
	nova, _ := os.ReadFile(AgentFile(root, "nova"))
	atlas, _ := os.ReadFile(AgentFile(root, "atlas"))
	if strings.Contains(string(main), "atlas") || strings.Contains(string(main), "nova") || !strings.Contains(string(main), "OPENROUTER_API_KEY") {
		t.Errorf("secrets.local.yaml:\n%s", main)
	}
	if !strings.Contains(string(nova), "nova-bot") || !strings.Contains(string(atlas), "API_SERVER_KEY: k") || !strings.Contains(string(atlas), "SWARM_CORE_KEY") {
		t.Errorf("nova:\n%s\natlas:\n%s", nova, atlas)
	}
	if gi, err := os.ReadFile(filepath.Join(root, "agents", "nova", "data", ".gitignore")); err != nil || !strings.Contains(string(gi), "*") {
		t.Error("nova's data folder is not ignored by git")
	}
	if Stamp(root) == before {
		t.Error("the stamp did not change")
	}
	// The same agent in both places is refused.
	write(Path(root), "agents:\n  atlas:\n    SLACK_BOT_TOKEN: other\n")
	if _, err := Load(Path(root)); err == nil || !strings.Contains(err.Error(), "both") {
		t.Errorf("both places: %v", err)
	}
}
