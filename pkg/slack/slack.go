// Package slack generates each agent's Slack app. One Slack app per agent (its own name and
// avatar), connected with Socket Mode so neither ECS nor a laptop needs a public URL. Run a
// separate "<Name> (dev)" app for local runs: Socket Mode spreads events across every connection
// on an app token, so a local container on the production app would silently take part of
// production's traffic (the secrets guard refuses that).
package slack

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

// SlashCommandOwners are the agents whose Slack app owns the native slash commands. Slack allows
// one owner per command name.
func SlashCommandOwners(inst *instance.Instance) ([]string, error) {
	owners := []string{}
	for _, id := range manifest.AgentIDs(inst.Root) {
		a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			return nil, err
		}
		for _, c := range a.Channels {
			if c.Kind == "slack" && c.SlashCommands != nil && *c.SlashCommands {
				owners = append(owners, id)
				break
			}
		}
	}
	return owners, nil
}

// applySlashPolicy strips the slash commands (and the scope they need) from a non-owner's app.
func applySlashPolicy(m *node, owner bool) {
	if owner {
		return
	}
	if f := m.get("features"); f != nil && f.kind == 'o' {
		f.del("slash_commands")
	}
	bot := m.get("oauth_config").get("scopes").get("bot")
	if bot != nil && bot.kind == 'a' {
		kept := []*node{}
		for _, s := range bot.vals {
			if v, ok := s.str(); ok && v == "commands" {
				continue
			}
			kept = append(kept, s)
		}
		bot.vals = kept
	}
}

// ApplySlashPolicy is applySlashPolicy on a JSON manifest (for callers and tests).
func ApplySlashPolicy(manifestJSON []byte, owner bool) ([]byte, error) {
	m, err := parseJSON(manifestJSON)
	if err != nil {
		return nil, err
	}
	applySlashPolicy(m, owner)
	return []byte(m.stringify()), nil
}

var notUsername = regexp.MustCompile(`[^A-Za-z0-9 ._-]`)

// BotName: Slack turns the bot display name into a username, which must be ASCII ("Renée" → "Renee").
func BotName(name string) string {
	var b strings.Builder
	for _, r := range name {
		if s, ok := stripAccents[r]; ok {
			b.WriteString(s)
		} else if r < 0x300 || r > 0x36f {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(notUsername.ReplaceAllString(b.String(), ""))
}

// AppName is the Slack app's name ("<Name> (dev)" for the local dev app).
func AppName(name string, dev bool) string {
	if dev {
		return name + " (dev)"
	}
	return name
}

// Runner runs a command and returns stdout, stderr and the exit code.
type Runner func(argv []string) (stdout, stderr string, code int)

// Exec is the real runner.
func Exec(argv []string) (string, string, int) {
	c := exec.Command(argv[0], argv[1:]...)
	var out, errb strings.Builder
	c.Stdout, c.Stderr = &out, &errb
	code := 0
	if err := c.Run(); err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			errb.WriteString(err.Error())
		}
	}
	return out.String(), errb.String(), code
}

// Result of Manifest: where the manifest was written and the setup steps to print.
type Result struct {
	Path  string
	Steps []string
}

func truncate(s string, n int) string {
	u := []rune(s)
	if len(u) > n {
		return string(u[:n])
	}
	return s
}

// manifestDrafter is an engine that drafts an agent's Slack app manifest with its own tooling: the
// command prints the manifest JSON (the scopes and events its gateway needs).
type manifestDrafter interface {
	SlackManifestArgv(a *manifest.Agent, name, description string) []string
}

// Manifest generates the agent's Slack app manifest with the pinned engine image's own generator
// and writes dist/<id>/slack-manifest[.dev].json.
func Manifest(inst *instance.Instance, id string, dev bool, run Runner) (*Result, error) {
	a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		return nil, err
	}
	hasSlack := false
	for _, c := range a.Channels {
		hasSlack = hasSlack || c.Kind == "slack"
	}
	if !hasSlack {
		return nil, fmt.Errorf("%s has no slack channel in agent.yaml", id)
	}
	eng, err := engines.Get(a.Engine.Kind, inst)
	if err != nil {
		return nil, err
	}
	drafter, ok := eng.(manifestDrafter)
	if !ok {
		return nil, fmt.Errorf("the %s engine cannot draft a Slack app manifest", eng.Kind())
	}
	name := AppName(a.Name, dev)
	// The TS engine cut the description at 139 UTF-16 units; roles are plain text, so runes match.
	argv := drafter.SlackManifestArgv(a, name, truncate(a.Role, 139))
	out, stderr, code := run(argv)
	start := strings.Index(out, "{")
	if code != 0 || start < 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = out
		}
		return nil, fmt.Errorf("%s slack manifest failed: %s", eng.Kind(), msg)
	}
	owners, err := SlashCommandOwners(inst)
	if err != nil {
		return nil, err
	}
	if len(owners) > 1 {
		return nil, fmt.Errorf("slash_commands is set for %s; Slack lets one app per workspace own a command name", strings.Join(owners, " and "))
	}
	m, err := parseJSON([]byte(out[start : strings.LastIndex(out, "}")+1]))
	if err != nil {
		return nil, fmt.Errorf("%s slack manifest: %w", eng.Kind(), err)
	}
	isOwner := false
	for _, o := range owners {
		isOwner = isOwner || o == id
	}
	applySlashPolicy(m, isOwner)
	if dn := m.get("features").get("bot_user").get("display_name"); dn != nil {
		if s, ok := dn.str(); ok && s != "" {
			dn.scalar = BotName(s)
		}
	}
	file := "slack-manifest.json"
	if dev {
		file = "slack-manifest.dev.json"
	}
	path := filepath.Join(inst.Root, "dist", id, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(m.stringify()+"\n"), 0o644); err != nil {
		return nil, err
	}
	slot := "agents." + id
	devWord, finish := "", fmt.Sprintf(" and `stormo secrets push %s --yes`", id)
	if dev {
		slot, devWord, finish = "local.agents."+id, "dev ", fmt.Sprintf(" and `stormo start %s`", id)
	}
	avatar := inst.Names.Resource + " personas/<slug>/avatar/"
	if a.Persona != nil {
		repo := a.Persona.Repo
		if repo == "" {
			repo = inst.Names.Resource
		}
		avatar = repo + "/" + a.Persona.Path + "/avatar/"
	}
	return &Result{Path: path, Steps: []string{
		fmt.Sprintf("1. https://api.slack.com/apps → Create New App → From an app manifest → pick the %sworkspace → paste %s", devWord, path),
		"2. Basic Information → App icon: upload the baked avatar from " + avatar,
		"3. Basic Information → App-Level Tokens → generate one with connections:write → xapp-… = SLACK_APP_TOKEN",
		"4. Install App → copy the Bot User OAuth Token xoxb-… = SLACK_BOT_TOKEN",
		fmt.Sprintf("5. Put both under %s in secrets.local.yaml, then `stormo secrets check %s`", slot, id) + finish,
		fmt.Sprintf("6. Invite the bot to its channels (/invite @%s); add Member IDs to channels[].allowed_users in agents/%s/agent.yaml", name, id),
	}}, nil
}
