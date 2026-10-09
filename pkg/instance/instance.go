// Package instance finds and loads a Stormo instance: one organisation's portable config
// directory (agents, units, personas, bridge actions, policy docs) marked by stormo.yaml at its
// root (docs/instances.md). The engine never names an organisation; everything org-specific is
// read from here. Runtime state (dist/, .swarm/, secrets.local.yaml) lives in the instance too.
package instance

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
)

// File marks an instance directory.
const File = "stormo.yaml"

// Unset is how a missing deploy setting renders; `deploy render` reports every one it finds.
const Unset = "<unset>"

// AwsTarget is the instance's AWS deploy target (stormo.yaml deploy.aws).
type AwsTarget struct {
	Account          string `json:"account"`
	Region           string `json:"region"`
	Cluster          string `json:"cluster"`
	Bucket           string `json:"bucket"`
	Registry         string `json:"registry"`
	ExecutionRoleArn string `json:"executionRoleArn"`
	TaskRoleArn      string `json:"taskRoleArn"`
	Efs              Efs    `json:"efs"`
}

// Efs is the shared document space: one file system, one access point per layer.
type Efs struct {
	FileSystemID string            `json:"fileSystemId"`
	AccessPoints map[string]string `json:"accessPoints"`
}

// Names are baked into deployed infrastructure and into the agents' own skills. Changing one
// renames an ECS service, a Secrets Manager id, a nap path or a skill the agents call by name,
// so instances pin them explicitly once anything is deployed.
type Names struct {
	// ECS family/service, log group, sidecar image, secret tags, generated-file headers.
	Resource string `json:"resource"`
	// Secrets Manager id prefix: <secret>/<agent>.
	Secret string `json:"secret"`
	// Engine-home directory for durable tool state (manifest `state:`).
	StateDir       string `json:"stateDir"`
	KnowledgeSkill string `json:"knowledgeSkill"`
	SharedSkill    string `json:"sharedSkill"`
}

// Desk is what sits on an agent's desk in the office UI unless its persona says otherwise.
type Desk struct {
	App     string   `json:"app,omitempty" yaml:"app"`
	Screens int      `json:"screens,omitempty" yaml:"screens"`
	Props   []string `json:"props,omitempty" yaml:"props"`
	Side    string   `json:"side,omitempty" yaml:"side"`
}

// OfficeUnit is how one unit's office looks in the office UI (all fields optional).
type OfficeUnit struct {
	Hue   string `json:"hue,omitempty" yaml:"hue"`     // CSS colour of the unit's name tags and chips
	Wall  string `json:"wall,omitempty" yaml:"wall"`   // map | board | moodboard | shelves
	Floor string `json:"floor,omitempty" yaml:"floor"` // wood | carpet | concrete | library
	Desk  *Desk  `json:"desk,omitempty" yaml:"desk"`
}

// ScrubRule is an instance PII rule: every match of Pattern (RE2) for which each Require pattern
// also matches is replaced by [KIND].
type ScrubRule struct {
	Kind    string   `json:"kind" yaml:"kind"`
	Pattern string   `json:"pattern" yaml:"pattern"`
	Require []string `json:"require,omitempty" yaml:"require"`
}

// Clock is one world clock on the office lobby wall.
type Clock struct {
	City string `json:"city" yaml:"city"`
	TZ   string `json:"tz" yaml:"tz"`
}

// CoreLearning is when the swarm core runs the learning cycle on its own (stormo.yaml
// core.learning): each agent napped, then dreamed into its working tree, one after another. Off
// unless At is set; accepting, rejecting and promoting stay human (`stormo learn`).
type CoreLearning struct {
	// Daily start time "HH:MM" in Timezone; "" never starts on its own.
	At string `json:"at,omitempty"`
	// IANA zone for At; "" is the host's.
	Timezone string `json:"timezone,omitempty"`
	// Minutes between two agents' turns in a scheduled cycle (default 2).
	StaggerMinutes int `json:"staggerMinutes"`
	// How long to wait for a busy agent to go quiet before dreaming without a fresh nap (default 30).
	QuietWaitMinutes int `json:"quietWaitMinutes"`
	// The agents a scheduled cycle covers; empty is every agent.
	Agents []string `json:"agents,omitempty"`
	// AutoAccept: after each dream, accept the lessons that need no judgement call: agent-scoped
	// memory (never user-profile) with no PII flag, seen at least AutoAcceptMinSeen times. Skills
	// and unit or group promotions always wait for a person.
	AutoAccept        bool `json:"autoAccept"`
	AutoAcceptMinSeen int  `json:"autoAcceptMinSeen"`
	// AutoRestart: restart a running, idle agent after lessons were auto-accepted, so they apply.
	AutoRestart bool `json:"autoRestart"`
}

// Instance is a loaded stormo.yaml.
type Instance struct {
	// Absolute path of the instance directory.
	Root string `json:"root"`
	// Display name: CLI banner, office UI, the ChatGPT sign-in page.
	Name string `json:"name"`
	// Organisation name used in text compiled into agents.
	Org string `json:"org"`
	// Lowercase id; the default for every name.
	Slug string `json:"slug"`
	// Author of generated engine distributions.
	Author string `json:"author"`
	Names  Names  `json:"names"`
	// Token prefixes (`<prefix>_…`) the PII scrubber treats as secrets, on top of the engine's.
	ScrubTokenPrefixes []string `json:"scrubTokenPrefixes"`
	// The instance's own PII rules (stormo.yaml scrub.rules), run after the engine's.
	ScrubRules []ScrubRule `json:"scrubRules"`
	// File (relative to the instance) declaring the instance's bridge actions.
	BridgeActions string  `json:"bridgeActions,omitempty"`
	Clocks        []Clock `json:"clocks"`
	// Office UI look per unit id (stormo.yaml office.units).
	OfficeUnits map[string]OfficeUnit `json:"officeUnits"`
	// The core's own learning schedule (stormo.yaml core.learning).
	CoreLearning CoreLearning `json:"coreLearning"`
	// Ways to reach models (stormo.yaml connections:, plus the implicit openrouter and chatgpt).
	Connections []Connection `json:"connections"`
	Aws         AwsTarget    `json:"aws"`
}

// Error is a problem with an instance's stormo.yaml or with finding one.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

var (
	slugRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	atRe   = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// ValidSlug reports whether s can be an instance slug: lowercase letters, digits and hyphens,
// starting with a letter.
func ValidSlug(s string) bool { return slugRe.MatchString(s) }

type rawFile struct {
	Name   string `yaml:"name"`
	Org    string `yaml:"org"`
	Slug   any    `yaml:"slug"`
	Author string `yaml:"author"`
	Names  struct {
		Resource       string `yaml:"resource"`
		Secret         string `yaml:"secret"`
		StateDir       string `yaml:"state_dir"`
		KnowledgeSkill string `yaml:"knowledge_skill"`
		SharedSkill    string `yaml:"shared_skill"`
	} `yaml:"names"`
	Scrub struct {
		TokenPrefixes []string    `yaml:"token_prefixes"`
		Rules         []ScrubRule `yaml:"rules"`
	} `yaml:"scrub"`
	Bridge struct {
		Actions string `yaml:"actions"`
	} `yaml:"bridge"`
	Office struct {
		Clocks []Clock               `yaml:"clocks"`
		Units  map[string]OfficeUnit `yaml:"units"`
	} `yaml:"office"`
	Core struct {
		Learning struct {
			At                string   `yaml:"at"`
			Timezone          string   `yaml:"timezone"`
			StaggerMinutes    *int     `yaml:"stagger_minutes"`
			QuietWaitMinutes  *int     `yaml:"quiet_wait_minutes"`
			Agents            []string `yaml:"agents"`
			AutoAccept        bool     `yaml:"auto_accept"`
			AutoAcceptMinSeen *int     `yaml:"auto_accept_min_seen"`
			AutoRestart       bool     `yaml:"auto_restart"`
		} `yaml:"learning"`
	} `yaml:"core"`
	Connections []rawConnection `yaml:"connections"`
	Deploy      struct {
		Aws struct {
			Account          any    `yaml:"account"`
			Region           string `yaml:"region"`
			Cluster          string `yaml:"cluster"`
			Bucket           string `yaml:"bucket"`
			Registry         string `yaml:"registry"`
			ExecutionRoleArn string `yaml:"execution_role_arn"`
			TaskRoleArn      string `yaml:"task_role_arn"`
			Efs              struct {
				FileSystemID string            `yaml:"file_system_id"`
				AccessPoints map[string]string `yaml:"access_points"`
			} `yaml:"efs"`
		} `yaml:"aws"`
	} `yaml:"deploy"`
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Load reads and validates <root>/stormo.yaml, filling defaults derived from the slug.
func Load(root string) (*Instance, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(abs, File)
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{fmt.Sprintf("%s: %v", path, err)}
	}
	var raw rawFile
	if err := yaml.Unmarshal(body, &raw); err != nil {
		return nil, &Error{fmt.Sprintf("%s: %v", path, err)}
	}
	slug, _ := raw.Slug.(string)
	if !slugRe.MatchString(slug) {
		return nil, &Error{fmt.Sprintf("%s: slug is required (lowercase, e.g. acme)", path)}
	}
	org := or(raw.Org, slug)
	aws := raw.Deploy.Aws
	account := Unset
	if aws.Account != nil {
		account = fmt.Sprint(aws.Account)
	}
	aps := aws.Efs.AccessPoints
	if aps == nil {
		aps = map[string]string{}
	}
	kindRe := regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	for i, r := range raw.Scrub.Rules {
		if !kindRe.MatchString(r.Kind) {
			return nil, &Error{fmt.Sprintf("%s: scrub.rules[%d].kind must be a lowercase word", path, i)}
		}
		for _, p := range append([]string{r.Pattern}, r.Require...) {
			if _, err := regexp.Compile(p); err != nil || r.Pattern == "" {
				return nil, &Error{fmt.Sprintf("%s: scrub.rules[%d] (%s): invalid pattern %q", path, i, r.Kind, p)}
			}
		}
	}
	cl := raw.Core.Learning
	learning := CoreLearning{At: cl.At, Timezone: cl.Timezone, StaggerMinutes: 2, QuietWaitMinutes: 30, Agents: cl.Agents,
		AutoAccept: cl.AutoAccept, AutoAcceptMinSeen: 1, AutoRestart: cl.AutoRestart}
	if cl.AutoAcceptMinSeen != nil {
		if *cl.AutoAcceptMinSeen < 1 || *cl.AutoAcceptMinSeen > 1000 {
			return nil, &Error{fmt.Sprintf("%s: core.learning.auto_accept_min_seen must be 1 to 1000", path)}
		}
		learning.AutoAcceptMinSeen = *cl.AutoAcceptMinSeen
	}
	if cl.AutoRestart && !cl.AutoAccept {
		return nil, &Error{fmt.Sprintf("%s: core.learning.auto_restart needs auto_accept", path)}
	}
	if cl.At != "" && !atRe.MatchString(cl.At) {
		return nil, &Error{fmt.Sprintf("%s: core.learning.at must be a 24-hour time like \"03:00\", got %q", path, cl.At)}
	}
	if cl.Timezone != "" {
		if _, err := time.LoadLocation(cl.Timezone); err != nil {
			return nil, &Error{fmt.Sprintf("%s: core.learning.timezone: %v", path, err)}
		}
	}
	for name, v := range map[string]*int{"stagger_minutes": cl.StaggerMinutes, "quiet_wait_minutes": cl.QuietWaitMinutes} {
		if v != nil && (*v < 0 || *v > 24*60) {
			return nil, &Error{fmt.Sprintf("%s: core.learning.%s must be 0 to 1440", path, name)}
		}
	}
	if cl.StaggerMinutes != nil {
		learning.StaggerMinutes = *cl.StaggerMinutes
	}
	if cl.QuietWaitMinutes != nil {
		learning.QuietWaitMinutes = *cl.QuietWaitMinutes
	}
	connections, err := connectionsOf(raw.Connections, path)
	if err != nil {
		return nil, err
	}
	rules := raw.Scrub.Rules
	if rules == nil {
		rules = []ScrubRule{}
	}
	prefixes := raw.Scrub.TokenPrefixes
	if prefixes == nil {
		prefixes = []string{}
	}
	return &Instance{
		Root:   abs,
		Name:   or(raw.Name, org+" Stormo"),
		Org:    org,
		Slug:   slug,
		Author: or(raw.Author, org),
		Names: Names{
			Resource:       or(raw.Names.Resource, slug+"-stormo"),
			Secret:         or(raw.Names.Secret, slug+"/stormo"),
			StateDir:       or(raw.Names.StateDir, slug+"-state"),
			KnowledgeSkill: or(raw.Names.KnowledgeSkill, slug+"-knowledge"),
			SharedSkill:    or(raw.Names.SharedSkill, slug+"-shared-docs"),
		},
		ScrubTokenPrefixes: prefixes,
		ScrubRules:         rules,
		BridgeActions:      raw.Bridge.Actions,
		Clocks:             raw.Office.Clocks,
		OfficeUnits:        orUnits(raw.Office.Units),
		CoreLearning:       learning,
		Aws: AwsTarget{
			Account:          account,
			Region:           or(aws.Region, Unset),
			Cluster:          or(aws.Cluster, Unset),
			Bucket:           or(aws.Bucket, Unset),
			Registry:         or(aws.Registry, Unset),
			ExecutionRoleArn: or(aws.ExecutionRoleArn, Unset),
			TaskRoleArn:      or(aws.TaskRoleArn, Unset),
			Efs:              Efs{FileSystemID: or(aws.Efs.FileSystemID, Unset), AccessPoints: aps},
		},
		Connections: connections,
	}, nil
}

func orUnits(u map[string]OfficeUnit) map[string]OfficeUnit {
	if u == nil {
		return map[string]OfficeUnit{}
	}
	return u
}

// Find locates the instance, first match wins: `--instance <dir>` in args, STORMO_INSTANCE, the
// nearest stormo.yaml at or above cwd, the nearest at or above the stormo binary (an engine built
// inside its instance's stormo/ checkout). Returns "" when there is none.
func Find(cwd string, getenv func(string) string, exe string, args []string) string {
	for i, a := range args {
		if a == "--instance" && i+1 < len(args) {
			return abs(cwd, args[i+1])
		}
		if v, ok := strings.CutPrefix(a, "--instance="); ok {
			return abs(cwd, v)
		}
	}
	if v := getenv("STORMO_INSTANCE"); v != "" {
		return abs(cwd, v)
	}
	starts := []string{cwd}
	if exe != "" {
		starts = append(starts, filepath.Dir(exe))
	}
	for _, start := range starts {
		for dir := abs(cwd, start); ; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, File)); err == nil {
				return dir
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return ""
}

func abs(cwd, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, p)
}

var (
	once    sync.Once
	current *Instance
	curErr  error
)

// Current resolves and loads this process's instance once (from os.Args, the environment and
// the working directory). Never guesses: an instance's names decide nap paths and secret ids.
func Current() (*Instance, error) {
	once.Do(func() {
		cwd, _ := os.Getwd()
		exe, _ := os.Executable()
		if exe != "" {
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				exe = real
			}
		}
		root := Find(cwd, os.Getenv, exe, os.Args[1:])
		if root == "" {
			curErr = &Error{fmt.Sprintf("no %s found: pass --instance <dir>, set STORMO_INSTANCE, or run inside an instance", File)}
			return
		}
		current, curErr = Load(root)
	})
	return current, curErr
}

// Must is Current for code paths that cannot run without an instance.
func Must() *Instance {
	i, err := Current()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stormo:", err)
		os.Exit(2)
	}
	return i
}
