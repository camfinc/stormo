package approvals

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Risky terminal commands (approvals.commands, docs/needletail.md §7): a command one of these
// matches needs a person's approval before it runs, in any conversation (a trusted one too, as
// Hermes asks before a dangerous command); in a scheduled run, where no person is in the loop, it
// is refused. The patterns are Go regular expressions over the whole command, matched case
// insensitively. They catch the obvious; they are not a sandbox (the jail and the uid split are).

// DefaultCommands are the patterns every agent gets; approvals.commands adds to them.
var DefaultCommands = []Rule{
	{Name: "recursive delete of the root, the home or the working directory", Pattern: `\brm\s+(-[a-z]*\s+)*-[a-z]*[rR][a-z]*\s+(-[a-z-]+\s+)*(--\s+)?("|')?(/|/\*|~|~/|~/\*|\$HOME|\$HOME/|\$HOME/\*|\$\{HOME\}/?|\.|\./|\./\*|\*|\.\.|\.\./?)("|')?(\s|;|&|\||$)`},
	{Name: "a download piped into a shell", Pattern: `\b(curl|wget|fetch)\b[^\n;&]*\|\s*(sudo\s+)?(env\s+\S+\s+)*(ba|z|da|k|fi)?sh\b`},
	{Name: "a shell running a download", Pattern: `\b(ba|z|da|k)?sh\s+(-c\s+)?("|')?\$\((curl|wget)\b|<\(\s*(curl|wget)\b`},
	{Name: "a force push", Pattern: `\bgit\b[^\n;&|]*\bpush\b[^\n;&|]*(\s--force(-with-lease)?\b|\s-[a-z]*f[a-z]*\b|\s\+\S)`},
	{Name: "credential files", Pattern: `(~|\$HOME|\$\{HOME\}|/)?[^\s]*(\.ssh/|\bid_(rsa|ed25519|ecdsa|dsa)\b|\.aws/credentials|\.netrc\b|\.git-credentials|\.npmrc\b|\.pypirc\b|\.docker/config\.json|\.kube/config|\.gnupg/|/proc/(self|\d+|1)/environ)`},
	{Name: "a filesystem or disk write", Pattern: `\bmkfs(\.\w+)?\b|\bdd\b[^\n;&|]*\bof=/dev/|>\s*/dev/(sd|nvme|xvd|vd)`},
	{Name: "a fork bomb", Pattern: `:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`},
	{Name: "world-writable recursive chmod", Pattern: `\bchmod\s+(-[a-z]*\s+)*-[a-z]*R[a-z]*\s+(-[a-z]+\s+)*0?777\b`},
}

// Rule is one risky-command pattern and what it means (needletail.yaml approvals.commands).
type Rule = wire.CommandRule

// Commands is a compiled set of rules.
type Commands struct {
	rules []compiled
}

type compiled struct {
	name string
	re   *regexp.Regexp
}

// CompileCommands compiles the default rules and extra (approvals.commands): each an RE2
// pattern, matched case insensitively.
func CompileCommands(extra []Rule) (*Commands, error) {
	c := &Commands{}
	for _, r := range append(append([]Rule{}, DefaultCommands...), extra...) {
		re, err := regexp.Compile(`(?i)` + r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("approvals.commands %q: %v", r.Pattern, err)
		}
		name := r.Name
		if strings.TrimSpace(name) == "" {
			name = "pattern " + r.Pattern
		}
		c.rules = append(c.rules, compiled{name: name, re: re})
	}
	return c, nil
}

// Match is the name of the first rule command matches ("" when none does).
func (c *Commands) Match(command string) string {
	if c == nil {
		return ""
	}
	for _, r := range c.rules {
		if r.re.MatchString(command) {
			return r.name
		}
	}
	return ""
}
