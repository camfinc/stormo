package needletail

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/camfinc/stormo/internal/needletail/commands"
	"github.com/camfinc/stormo/pkg/manifest"
)

// The Slack app manifest a needletail agent needs (stormo slack manifest, pkg/slack): Socket Mode,
// Interactivity (the approval cards' buttons), the events its gateway hears, the scopes of the Web
// API methods it calls, and its slash commands under their configured names (slack.commands in
// engine/needletail/config.yaml; pkg/slack strips them, and the commands scope, from an app that
// does not own them). Rendered in process: no image is run.

// slackBotScopes are the scopes of what the gateway calls: chat.postMessage, chat.update and
// chat.postEphemeral (chat:write), reactions.add/remove (reactions:write), the thread status line
// (assistant:write), conversations.replies (the *:history scopes), users.info (users:read),
// conversations.info and conversations.list (the *:read scopes), conversations.open (im:write),
// file downloads (files:read), mentions (app_mentions:read) and slash commands (commands).
var slackBotScopes = []string{"app_mentions:read", "assistant:write", "channels:history", "channels:read", "chat:write", "commands", "files:read",
	"groups:history", "groups:read", "im:history", "im:read", "im:write", "mpim:history", "mpim:read", "reactions:write", "users:read"}

// slackBotEvents are the events the gateway hears.
var slackBotEvents = []string{"app_mention", "message.channels", "message.groups", "message.im", "message.mpim"}

// SlackManifest renders the agent's Slack app manifest (pkg/slack's in-process renderer).
func (n *Needletail) SlackManifest(a *manifest.Agent, name, description string) ([]byte, error) {
	ac, err := readAgentConfig(manifest.AgentDir(n.inst.Root, a.ID))
	if err != nil {
		return nil, fmt.Errorf("agents/%s: %w", a.ID, err)
	}
	prefix, renamed := "", map[string]string(nil)
	if ac.Slack.Commands != nil {
		prefix, renamed = ac.Slack.Commands.Prefix, ac.Slack.Commands.Names
	}
	names, err := commands.SlackNames(prefix, renamed)
	if err != nil {
		return nil, fmt.Errorf("agents/%s: %s: slack.commands: %w", a.ID, ConfigFile, err)
	}
	return SlackManifestJSON(name, description, names)
}

// SlackManifestJSON is the manifest for an app called name, its commands' names (key → name).
func SlackManifestJSON(name, description string, names map[string]string) ([]byte, error) {
	slashes := []map[string]any{}
	for _, d := range commands.Defs {
		c := map[string]any{"command": "/" + names[d.Key], "description": d.Description, "should_escape": false,
			// required by the schema, never called: Socket Mode delivers the commands
			"url": "https://needletail.invalid/slack/commands"}
		if d.Usage != "" {
			c["usage_hint"] = d.Usage
		}
		slashes = append(slashes, c)
	}
	if description == "" {
		description = name + ", a needletail agent"
	}
	scopes := append([]string{}, slackBotScopes...)
	sort.Strings(scopes)
	m := map[string]any{
		"_metadata":           map[string]any{"major_version": 1, "minor_version": 1},
		"display_information": map[string]any{"name": clip(name, 35), "description": clip(description, 140)},
		"features": map[string]any{
			"app_home":       map[string]any{"home_tab_enabled": false, "messages_tab_enabled": true, "messages_tab_read_only_enabled": false},
			"bot_user":       map[string]any{"display_name": clip(name, 80), "always_online": true},
			"assistant_view": map[string]any{"assistant_description": clip(description, 140)},
			"slash_commands": slashes,
		},
		"oauth_config": map[string]any{"scopes": map[string]any{"bot": scopes}},
		"settings": map[string]any{
			"event_subscriptions":    map[string]any{"bot_events": slackBotEvents},
			"interactivity":          map[string]any{"is_enabled": true},
			"org_deploy_enabled":     false,
			"socket_mode_enabled":    true,
			"token_rotation_enabled": false,
		},
	}
	return json.MarshalIndent(m, "", "  ")
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
