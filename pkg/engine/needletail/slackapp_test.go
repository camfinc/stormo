package needletail

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/commands"
)

// The app manifest: Socket Mode and Interactivity on, the events and scopes the gateway uses (a
// reaction needs reactions:write, the status line assistant:write), the commands under their
// configured names (status as agent-status: Slack reserves /status).
func TestSlackManifest(t *testing.T) {
	inst, a := atlas(t)
	n := New(inst)
	body, err := n.SlackManifest(a, "Atlas", "Answers the team")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Features struct {
			SlashCommands []struct {
				Command, Description, URL string
			} `json:"slash_commands"`
		} `json:"features"`
		OAuth struct {
			Scopes struct{ Bot []string } `json:"scopes"`
		} `json:"oauth_config"`
		Settings struct {
			Events struct {
				Bot []string `json:"bot_events"`
			} `json:"event_subscriptions"`
			Interactivity struct {
				On bool `json:"is_enabled"`
			} `json:"interactivity"`
			Socket bool `json:"socket_mode_enabled"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	cmds := []string{}
	for _, c := range m.Features.SlashCommands {
		cmds = append(cmds, c.Command)
	}
	if !slices.Equal(cmds, []string{"/new", "/agent-status", "/memory", "/jobs", "/stop", "/trust", "/help"}) {
		t.Fatalf("commands %v", cmds)
	}
	for _, s := range []string{"chat:write", "reactions:write", "assistant:write", "files:read", "im:write", "commands", "users:read", "app_mentions:read"} {
		if !slices.Contains(m.OAuth.Scopes.Bot, s) {
			t.Errorf("scope %s missing: %v", s, m.OAuth.Scopes.Bot)
		}
	}
	if !m.Settings.Socket || !m.Settings.Interactivity.On || !slices.Contains(m.Settings.Events.Bot, "message.im") {
		t.Fatalf("settings %+v", m.Settings)
	}
	names, _ := commands.SlackNames("ops-", nil)
	body, _ = SlackManifestJSON("Atlas", "", names)
	if err := json.Unmarshal(body, &m); err != nil || m.Features.SlashCommands[1].Command != "/ops-status" {
		t.Fatalf("prefixed: %v %+v", err, m.Features.SlashCommands)
	}
}
