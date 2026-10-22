package cli

import (
	"fmt"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/luau"
)

// MaxGuide bounds the system prompt's guide section (bytes): it is in every turn.
const MaxGuide = 7000

// guide is what the system prompt's guide section is made from (docs/needletail.md §4.4): it is
// generated from the runtime's own settings and the Luau registry, not written by hand, so it
// says what this runtime does.
type guide struct {
	Home string
	// Shared are the shared-space directories (group, then the unit's).
	Shared []string
	// Split: the agent's commands run as their own user, so the runtime's files are out of
	// their reach (§7); otherwise they are protected only by the tools.
	Split bool
	// Luau: the Luau engine runs; Grants are the agent's grants, LuauTimeout a run's bound (s).
	Luau        bool
	Grants      []string
	LuauTimeout int
	// Slack: the gateway runs (send_message, no_reply); Broadcast the agent's grant.
	Slack     bool
	Broadcast bool
}

// render is the guide section: where things are and which call changes what, how to write Luau
// tools and handlers (with the compact function reference), and how messages work.
func (g guide) render() string {
	var b strings.Builder
	b.WriteString("## Your home\n")
	fmt.Fprintf(&b, "Your home is %s: the terminal's working directory and $HOME.\n", g.Home)
	b.WriteString("- `SOUL.md`: who you are. Read-only; it changes through review.\n" +
		"- `skills/<category>/<name>/`: your skills (SKILL.md, scripts, `tools/*.luau`, `handlers/*.luau`). Change them with skill_manage, which checks them; the terminal may work in them too.\n" +
		"- `scripts/`: the scripts your scheduled jobs run. Write one, then name it in cronjob_manage.\n" +
		"- `memories/MEMORY.md`, `memories/USER.md`: your memory (above). Change it only with the memory tool.\n" +
		"- `cron/jobs.json`: your scheduled jobs. Change them only with cronjob_manage.\n" +
		"- `sessions/`, `logs/`, `state/`, `.swarm/`, `needletail.yaml`: the runtime's own. Never edit them.\n")
	if len(g.Shared) > 0 {
		fmt.Fprintf(&b, "- `%s`", g.Shared[0])
		if len(g.Shared) > 1 {
			fmt.Fprintf(&b, " (everyone) and `%s` (your unit)", g.Shared[1])
		}
		b.WriteString(": shared documents. Never use another unit's directory.\n")
	}
	if g.Split {
		b.WriteString("Your terminal commands and job scripts run as their own user: memories/, cron/, sessions/, logs/, state/, .swarm/ " +
			"and needletail.yaml are out of their reach (memories/ read-only). Use the tools named above for them. Run a script with its " +
			"interpreter (`bash x.sh`, `python3 x.py`): a file the runtime wrote for you cannot be chmod-ed by your commands.\n")
	}
	b.WriteString("\n")
	if g.Luau {
		b.WriteString("## Luau tools and handlers\n" +
			"Extend yourself with sandboxed Luau (no processes, env or secrets; only the functions below). skill_manage checks every " +
			"`.luau` file it writes; luau_test dry-runs one (writes, http, core calls and sends refused). A **tool**, " +
			"`skills/<skill>/tools/<name>.luau`, is offered to you as `<skill>__<name>` from your next turn:\n" +
			"```lua\n--[[needletail\nformat: 1\nname: fare_quote\ndescription: Quote a route's fare.\n" +
			"args: {type: object, properties: {route: {type: string}}, required: [route]}\ncaps: [http:api.example.com]\n]]\n" +
			"return function(args)\n  local r = http_request{url = \"https://api.example.com/fares?route=\" .. args.route}\n  return json_decode(r.body)\nend\n```\n" +
			"A **handler**, `skills/<skill>/handlers/<name>.luau`, runs before or instead of a turn; it declares `events:` (no args) and " +
			"defines a function per event:\n" +
			"```lua\n--[[needletail\nformat: 1\nname: hours\ndescription: Answers opening hours.\nevents: [on_message]\n]]\n" +
			"function on_message(ev)\n  if string.find(ev.text, \"hours\") then return {action = \"reply\", text = \"We open at 9.\"} end\n" +
			"  return nil -- pass: you answer\nend\n```\n")
		for _, d := range luau.EventDocs {
			fmt.Fprintf(&b, "- `%s`: answers %s.\n", d.Event, d.Actions)
		}
		b.WriteString("Handlers run in order (skill dir, then name); one that fails is skipped. on_schedule names its jobs " +
			"(`jobs: [<id or name>]`). Event fields and the rest: luau_reference.\n")
		grants := "none"
		if len(g.Grants) > 0 {
			grants = strings.Join(g.Grants, ", ")
		}
		fmt.Fprintf(&b, "A call needs a cap the header's `caps:` declares and you are granted (your grants: %s; http:, message: and "+
			"fs.*:shared need one from your owner). Each run: %d s (a header's `timeout:` may only lower it), 48 MiB, 8 MiB read and "+
			"4 MiB written. Functions:\n", grants, g.LuauTimeout)
		b.WriteString(luau.CompactReference())
		b.WriteString("\n")
	}
	if g.Slack {
		b.WriteString("## Messages\n" +
			"Your reply in a conversation is posted there. send_message posts elsewhere: `slack` (the home channel), a channel or " +
			"thread you are in, or the DM of a person allowed to talk to you (action list shows them); nothing else. ")
		if g.Broadcast {
			b.WriteString("@channel, @here, @everyone and user groups ping only with broadcast: true. ")
		} else {
			b.WriteString("You are not granted broadcasts: @channel, @here, @everyone and user groups never ping. ")
		}
		b.WriteString("A scheduled run's final reply is its delivery: do not send it yourself.\n\n")
	}
	return b.String()
}
