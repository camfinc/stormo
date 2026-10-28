package slack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine/hermes/hermestest"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestBotNameIsASCII(t *testing.T) {
	for in, want := range map[string]string{"Renée": "Renee", "Zoë (dev)": "Zoe dev", "Atlas": "Atlas", "Ñandú Ågé": "Nandu Age"} {
		if got := BotName(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestSlashPolicyKeepsOrderAndStripsNonOwners(t *testing.T) {
	in := `{"display_information":{"name":"X"},"features":{"bot_user":{"display_name":"Renée"},"slash_commands":[{"command":"/new"}]},"oauth_config":{"scopes":{"bot":["chat:write","commands","im:history"]}}}`
	out, err := ApplySlashPolicy([]byte(in), false)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "slash_commands") || strings.Contains(s, `"commands"`) {
		t.Fatal(s)
	}
	if strings.Index(s, "display_information") > strings.Index(s, "oauth_config") {
		t.Fatal("key order lost")
	}
	if kept, _ := ApplySlashPolicy([]byte(in), true); !strings.Contains(string(kept), "slash_commands") {
		t.Fatal("owner lost its commands")
	}
}

func TestManifestUsesTheEngineGenerator(t *testing.T) {
	// Hermes drafts the app manifest; atlas (on Hermes) owns the slash commands. A temporary
	// copy, so dist/ never lands in the example.
	dir := hermestest.Instance(t)
	inst, err := instance.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	var argv []string
	run := func(a []string) (string, string, int) {
		argv = a
		return "notice\n" + `{"features":{"bot_user":{"display_name":"Scout"},"slash_commands":[]},"oauth_config":{"scopes":{"bot":["commands","chat:write"]}}}` + "\n", "", 0
	}
	r, err := Manifest(inst, "scout", true, run)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), "--name Scout (dev)") || argv[5] != "nousresearch/hermes-agent:v2026.9.24" {
		t.Fatalf("argv %v", argv)
	}
	if r.Path != filepath.Join(dir, "dist", "scout", "slack-manifest.dev.json") {
		t.Fatal(r.Path)
	}
	body, _ := os.ReadFile(r.Path)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	// atlas owns the slash commands, so scout's app ships without them.
	if _, ok := m["features"].(map[string]any)["slash_commands"]; ok {
		t.Fatal("non-owner kept slash commands")
	}
	if !strings.Contains(r.Steps[4], "local.agents.scout") {
		t.Fatal(r.Steps[4])
	}
	if _, err := Manifest(inst, "scout", false, func([]string) (string, string, int) { return "", "no image", 1 }); err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatal(err)
	}
}

// renderer is an engine that renders its Slack app manifest in process (manifestRenderer).
type renderer struct{ asked *[]string }

func (renderer) Kind() string { return "fake" }
func (r renderer) SlackManifest(a *manifest.Agent, name, description string) ([]byte, error) {
	*r.asked = append(*r.asked, a.ID+"|"+name)
	return []byte(`{"features":{"bot_user":{"display_name":"` + name + `"},"slash_commands":[{"command":"/new"}]},"oauth_config":{"scopes":{"bot":["chat:write","commands"]}}}`), nil
}

// An engine that renders its manifest in process is asked before any image runs; the slash policy
// still applies (atlas owns the commands; scout's app ships without them or the commands scope).
func TestManifestRenderedInProcess(t *testing.T) {
	dir := hermestest.Instance(t)
	inst, err := instance.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	asked := []string{}
	ran := false
	run := func([]string) (string, string, int) { ran = true; return "", "", 1 }
	for _, id := range []string{"atlas", "scout"} {
		a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			t.Fatal(err)
		}
		r, err := manifestFor(inst, a, renderer{&asked}, false, run)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := os.ReadFile(r.Path)
		owner := strings.Contains(string(body), "slash_commands") && strings.Contains(string(body), `"commands"`)
		if owner != (id == "atlas") {
			t.Fatalf("%s: commands kept %v:\n%s", id, owner, body)
		}
	}
	if ran || len(asked) != 2 {
		t.Fatalf("ran an image %v; asked %v", ran, asked)
	}
}
