package slack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
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
	src, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	// Work on a copy so dist/ never lands in the example.
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(src.Root)); err != nil {
		t.Fatal(err)
	}
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
