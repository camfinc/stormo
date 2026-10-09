package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
)

// acme is a scratch copy of the example instance, so writes never touch examples/minimal.
func acme(t *testing.T) *instance.Instance {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("../../examples/minimal")); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func onDisk(t *testing.T, inst *instance.Instance, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(inst.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShowAndWriteKeepComments(t *testing.T) {
	inst := acme(t)
	f, err := Show(inst, "agents/atlas/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "agent" || f.Agent != "atlas" || f.Hash != Hash([]byte(f.Text)) || !strings.Contains(f.Text, "#") {
		t.Fatalf("%+v", f)
	}
	edited := strings.Replace(f.Text, "\nrole: ", "\n# a comment the editor added\nrole: ", 1)
	w, err := Write(inst, f.Path, []byte(edited), f.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if got := onDisk(t, inst, f.Path); got != edited || w.Hash != Hash([]byte(edited)) {
		t.Errorf("written %q, hash %s", got, w.Hash)
	}
	// The old hash is now stale.
	if _, err := Write(inst, f.Path, []byte(f.Text), f.Hash); code(err) != "conflict" {
		t.Errorf("stale hash: %v", err)
	}
	if got := onDisk(t, inst, f.Path); got != edited {
		t.Error("a refused write changed the file")
	}
	if entries, _ := os.ReadDir(filepath.Join(inst.Root, "agents/atlas")); len(entries) == 0 {
		t.Fatal("no files")
	} else {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".agent.yaml.") {
				t.Errorf("temporary file left behind: %s", e.Name())
			}
		}
	}
}

func TestWriteRefusesWhatWouldNotLoad(t *testing.T) {
	inst := acme(t)
	f, _ := Show(inst, "agents/atlas/agent.yaml")
	cases := map[string]struct{ body, code string }{
		"bad yaml":         {"id: atlas\nname: [unclosed\n", "invalid"},
		"unknown unit":     {strings.Replace(f.Text, "unit: sales", "unit: nowhere", 1), "invalid"},
		"secret in env":    {f.Text + "\nenv:\n  API_TOKEN: x\n", "invalid"},
		"foreign action":   {strings.Replace(f.Text, "actions:\n  - sales.crm.deals.list", "actions:\n  - support.tickets.list", 1), "invalid"},
		"deploy edited":    {strings.Replace(f.Text, "cpu: 1024", "cpu: 2048", 1), "readonly"},
		"no if-hash given": {f.Text, "usage"},
	}
	for name, c := range cases {
		if c.body == f.Text && name != "no if-hash given" {
			t.Fatalf("%s: the fixture changed, the case edits nothing", name)
		}
		hash := f.Hash
		if c.code == "usage" {
			hash = ""
		}
		if _, err := Write(inst, f.Path, []byte(c.body), hash); code(err) != c.code {
			t.Errorf("%s: want %s, got %v", name, c.code, err)
		}
		if onDisk(t, inst, f.Path) != f.Text {
			t.Fatalf("%s: a refused write changed the file", name)
		}
	}
}

func TestSoul(t *testing.T) {
	inst := acme(t)
	f, err := Show(inst, "agents/atlas/SOUL.md")
	if err != nil || f.Kind != "soul" {
		t.Fatal(f, err)
	}
	if _, err := Write(inst, f.Path, []byte(" \n"), f.Hash); code(err) != "invalid" {
		t.Errorf("empty soul: %v", err)
	}
	if _, err := Write(inst, f.Path, []byte(f.Text+"\nBe brief.\n"), f.Hash); err != nil {
		t.Fatal(err)
	}
}

func TestOnlyAgentFiles(t *testing.T) {
	inst := acme(t)
	for _, rel := range []string{"stormo.yaml", "agents/atlas/../../stormo.yaml", "agents/ghost/agent.yaml",
		"agents/atlas/hermes/config.base.yaml", "/etc/passwd", "../agents/atlas/agent.yaml"} {
		if _, err := Show(inst, rel); code(err) != "unsupported" {
			t.Errorf("%s: %v", rel, err)
		}
	}
}

func apply(t *testing.T, inst *instance.Instance, patch string) (before, after string, err error) {
	t.Helper()
	f, err := Show(inst, "agents/atlas/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(inst, f.Path, []byte(patch), f.Hash)
	return f.Text, onDisk(t, inst, f.Path), err
}

// changes lists the lines that differ, as "-old" and "+new", in file order (a minimal line diff).
func changes(before, after string) []string {
	b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
	i, j := 0, 0
	for i < len(b) && j < len(a) && b[i] == a[j] {
		i, j = i+1, j+1
	}
	k, l := len(b), len(a)
	for k > i && l > j && b[k-1] == a[l-1] {
		k, l = k-1, l-1
	}
	out := []string{}
	for _, x := range b[i:k] {
		out = append(out, "-"+x)
	}
	for _, x := range a[j:l] {
		out = append(out, "+"+x)
	}
	return out
}

func TestApplyKeepsTheFileAsWritten(t *testing.T) {
	cases := []struct {
		name, patch string
		want        []string
	}{
		{"nothing", `{}`, []string{}},
		{"same values", `{"name":"Atlas","learning":{"seed_fill":0.6},"memory":{"agent":2200}}`, []string{}},
		{"one scalar keeps its comment", `{"model":{"name":"openai/gpt-7"}}`,
			[]string{"-  name: openai/gpt-6-luna    # the cloud model", "+  name: openai/gpt-7 # the cloud model"}},
		{"an integer stays an integer", `{"memory":{"agent":3000}}`,
			[]string{"-  agent: 2200", "+  agent: 3000"}},
		{"null deletes", `{"learning":{"seed_fill":null}}`, []string{"-  seed_fill: 0.6"}},
		{"a list loses one item", `{"secrets":["OPENROUTER_API_KEY","SLACK_BOT_TOKEN","SLACK_APP_TOKEN","ELEVENLABS_API_KEY","API_SERVER_KEY"]}`,
			[]string{"-  - APIFY_API_TOKEN"}},
		{"a flow list stays flow", `{"channels":[{"kind":"slack","allow_bots":"mentions","slash_commands":true,"allowed_users":["U0000000001","U0000000002"],"home_channel":"C0000000001"},{"kind":"telegram"}]}`,
			[]string{"-    allowed_users: [U0000000001]", "+    allowed_users: [U0000000001, U0000000002]"}},
		{"a key leaves a list item", `{"channels":[{"kind":"slack","allow_bots":"mentions","allowed_users":["U0000000001"],"home_channel":"C0000000001"},{"kind":"telegram"}]}`,
			[]string{"-    slash_commands: true"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, after, err := apply(t, acme(t), c.patch)
			if err != nil {
				t.Fatal(err)
			}
			if got := changes(before, after); !slices.Equal(got, c.want) {
				t.Errorf("changes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}

func TestApplyNewSection(t *testing.T) {
	// Its own section after a blank line; a string that would read as a number stays a string.
	before, after, err := apply(t, acme(t), `{"env":{"BOT_MODE":"dry-run","RETRIES":"3"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if want := before + "\nenv:\n  BOT_MODE: dry-run\n  RETRIES: \"3\"\n"; after != want {
		t.Errorf("got\n%s", after[len(before)-40:])
	}
}

func TestApplyRefuses(t *testing.T) {
	for patch, want := range map[string]string{
		`{"deploy":{"cpu":2048}}`:              "readonly",
		`{"id":"zeus"}`:                        "readonly",
		`{"unit":"nowhere"}`:                   "invalid",
		`["not","an","object"]`:                "usage",
		`{"engine":{"local":{"via":"cloud"}}}`: "invalid",
	} {
		inst := acme(t)
		before, after, err := apply(t, inst, patch)
		if code(err) != want || before != after {
			t.Errorf("%s: want %s, got %v (file changed: %v)", patch, want, err, before != after)
		}
	}
	inst := acme(t)
	if _, err := Apply(inst, "agents/atlas/SOUL.md", []byte(`{}`), "x"); code(err) != "unsupported" {
		t.Errorf("SOUL.md patch: %v", err)
	}
}

func TestShowOffersTheFormsChoices(t *testing.T) {
	f, err := Show(acme(t), "agents/atlas/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := f.Doc.(map[string]any)
	o := f.Options
	if doc["unit"] != "sales" || doc["memory"].(map[string]any)["agent"] != 2200 {
		t.Errorf("doc = %v", doc)
	}
	units := []string{}
	for _, u := range o.Units {
		units = append(units, u.ID)
	}
	if !slices.Equal(units, []string{"group", "sales", "support"}) || !slices.Contains(o.Engines, "hermes") ||
		!slices.Contains(o.Skills, "crm/crm-api") || !slices.Contains(o.Personas, "personas/atlas") ||
		!slices.Contains(o.Secrets, "CRM_API_TOKEN") || !slices.Equal(o.AllowBots, []string{"none", "mentions", "all"}) ||
		len(o.Channels) != 2 || o.Channels[0].Kind != "slack" {
		t.Errorf("options = %+v", o)
	}
	if !slices.ContainsFunc(o.Actions, func(a ActionOption) bool { return a.Name == "sales.crm.deals.list" && a.Unit == "sales" }) {
		t.Errorf("actions = %+v", o.Actions)
	}
	if soul, _ := Show(acme(t), "agents/atlas/SOUL.md"); soul.Doc != nil || soul.Options != nil {
		t.Error("SOUL.md has no form")
	}
}
