// Package skill installs Stormo as an agent skill for Claude Code and Codex: one SKILL.md with
// references (the command list and the instance standard), in each tool's skills directory.
package skill

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	stormo "github.com/camfinc/stormo"
)

// Name is the skill's directory and name.
const Name = "stormo"

const marker = "<!-- installed by stormo "

//go:embed files/SKILL.md
var skillTemplate string

// Tool is an agent tool that reads skills.
type Tool string

const (
	Claude Tool = "claude"
	Codex  Tool = "codex"
)

// Tools lists the supported tools.
var Tools = []Tool{Claude, Codex}

// Scope is where a skill is installed: for the user, or for one project.
type Scope string

const (
	User    Scope = "user"
	Project Scope = "project"
)

// Dir is the skill's directory for a tool and scope. home is the user's home; root the project.
func Dir(t Tool, s Scope, home, root string) (string, error) {
	base := map[Tool]string{Claude: ".claude/skills", Codex: ".agents/skills"}[t]
	if base == "" {
		return "", fmt.Errorf("unknown tool %q (claude, codex)", t)
	}
	switch s {
	case User:
		return filepath.Join(home, base, Name), nil
	case Project:
		return filepath.Join(root, base, Name), nil
	}
	return "", fmt.Errorf("unknown scope %q (user, project)", s)
}

// Files are the skill's files, relative to its directory.
func Files(version, commands string) map[string]string {
	return map[string]string{
		"SKILL.md":                strings.ReplaceAll(skillTemplate, "{{VERSION}}", version),
		"references/commands.md":  "# stormo commands\n\nThe output of `stormo --help`.\n\n```\n" + strings.TrimSpace(commands) + "\n```\n",
		"references/instances.md": stormo.InstancesDoc,
	}
}

// ErrForeign: the directory holds a skill stormo did not write.
var ErrForeign = errors.New("holds a skill stormo did not install")

func ours(dir string) (exists bool, owned bool) {
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		_, statErr := os.Stat(dir)
		return statErr == nil, false
	}
	return true, strings.Contains(string(b), marker)
}

// Install writes the skill into dir, replacing an earlier stormo install. A directory holding any
// other skill is left alone unless force.
func Install(dir string, files map[string]string, force bool) error {
	if exists, owned := ours(dir); exists && !owned && !force {
		return fmt.Errorf("%s %w; pass --force to replace it", dir, ErrForeign)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(files[n]), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Uninstall removes a stormo-installed skill; reports false when there was none.
func Uninstall(dir string, force bool) (bool, error) {
	exists, owned := ours(dir)
	if !exists {
		return false, nil
	}
	if !owned && !force {
		return false, fmt.Errorf("%s %w; pass --force to remove it", dir, ErrForeign)
	}
	return true, os.RemoveAll(dir)
}
