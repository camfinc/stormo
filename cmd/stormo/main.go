// Command stormo operates an instance's agents: build, run (locally or on ECS), deploy, bench, and
// the nap/dream learning loop. The same binary runs inside the sidecars (rehydrate, nap).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	// Time zones (core.learning.timezone) resolve in the sidecar image too, which ships no zoneinfo.
	_ "time/tzdata"

	"github.com/camfinc/stormo"
	"github.com/camfinc/stormo/pkg/agentpack"
	"github.com/camfinc/stormo/pkg/bench"
	"github.com/camfinc/stormo/pkg/bridge"
	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/config"
	"github.com/camfinc/stormo/pkg/core"
	"github.com/camfinc/stormo/pkg/core/llm"
	"github.com/camfinc/stormo/pkg/deploy"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/inspect"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/models"
	"github.com/camfinc/stormo/pkg/ops"
	"github.com/camfinc/stormo/pkg/place"
	"github.com/camfinc/stormo/pkg/review"
	"github.com/camfinc/stormo/pkg/scaffold"
	"github.com/camfinc/stormo/pkg/secrets"
	"github.com/camfinc/stormo/pkg/shared"
	"github.com/camfinc/stormo/pkg/skill"
	"github.com/camfinc/stormo/pkg/slack"
	"github.com/camfinc/stormo/pkg/version"
	"github.com/spf13/pflag"
)

const usage = `stormo: manage %s agents (instance %s). Local containers by default; --remote (-r) for ECS.

  stormo                                  status of every agent (local; -r for ECS)
  stormo start   [agent...]               local: build + start containers · remote: desired count 1
  stormo stop    [agent...]               local: stop, final nap kept      · remote: desired count 0
  stormo restart [agent...]               local: recycle like a redeploy   · remote: new deployment
  stormo handoff <agent> --to local|remote  stop where it runs, carry its latest nap over, start there [--force]
  stormo learn   [agent...]               nap now (local), fold naps into proposals, show what to review
  stormo logs <agent> [-f] · chat <agent> "msg" [--session s] · nap-now <agent>   (local)
    target: --remote | --target local|remote | SWARM_TARGET=remote · remote changes ask first (--yes skips)
    start/restart (local): [--keep-home] [--nap-interval s] [--allow-prod-token] [--rebuild-sidecar]
    agents default to all of them
    instance: --instance <dir> | STORMO_INSTANCE | the nearest stormo.yaml above the working directory

  review learnings:
  learn list <agent> [--status proposed|accepted|rejected]
  learn accept|reject|pin|unpin <agent> <id...>
  learn edit <agent> <id> --text "..."   rewrite a learning (clears its PII flag)
  learn promote <agent> <id...> --scope unit|group
  learn skills <agent>                   list skill proposals
  learn skill-accept|skill-reject <agent> <skill-dir>

  build and ship:
  check [agent...]                       validate manifests, compile, check action isolation
  config show <file>                     print agents/<id>/agent.yaml or agents/<id>/SOUL.md
  config write <file> --if-hash h        replace it with stdin if unchanged since show and valid
  config apply <file> --if-hash h        change agent.yaml by a JSON merge patch on stdin, comments kept
  connections [list|kinds]               ways to reach models (stormo.yaml connections:) and who uses them
  connections models <name> [--agent id] the models a connection offers (its key: the agent's, else shared)
  connections add <name> --kind chatgpt|openrouter|openai|anthropic|custom [--base-url u] [--key NAME]
  connections remove <name>              refused while an agent uses it
  export <agent> [--data] [-o file.zip]  the agent as one zip; --data adds its naps and SECRET VALUES
  import <file.zip> [--as id] [--unit u] [--replace] [--with-actions]
                                         an exported agent into this instance; checked, undone if it fails
  migrate agent [agent...]               bring agents to agent.yaml format 1 (docs/agent-standard.md);
                                         a stopped agent's data moves into agents/<id>/data too
  build <agent>                          compile dist/<agent>/baseline (also a Hermes distribution)
  inspect <agent> [--target aws|local]   everything the engine computes for the agent, as JSON
  bench <agent> [--runner mock|docker] [--env-file f] [--tag t]   docker: env from secrets.local.yaml
  deploy render <agent> [--sidecar-tag t] write dist/<agent>/{taskdef,task-policy}.json, print aws commands
  deploy render-shared                   print the one-time EFS setup for the shared document space
  shared [--dir workdir]                 create the local shared space (workdir/<layer>, moved from .swarm/shared)

  slack manifest <agent> [--dev]         generate the agent's Slack app manifest (+ setup steps)

  review [agent...] [--since 24h]        read-only fleet health sweep (.swarm/review/)
  secrets init                           create/extend secrets.local.yaml with every declared name
  secrets set <shared|agent> NAME        set one value, read from stdin (never an argument)
  secrets share NAME... [--from <agent>] keep one value under shared: and drop identical per-agent copies
  secrets check [agent...]               report missing values (aws and local) and file problems
  secrets env <agent>                    write agents/<agent>/data/agent.env (local overlay applied)
  secrets push [agent...] [--yes]        diff key names vs AWS Secrets Manager; --yes writes

  dream <agent> [--store s3://bucket|dir] fold naps from an explicit store (learn does this for you)

%s
  sidecar (inside the task; env SWARM_AGENT, SWARM_STORE, SWARM_HOME):
  rehydrate                              build the engine home from baseline + latest nap
  nap [--loop]                           snapshot the engine home to the store

  agent skill (teaches Claude Code and Codex to operate stormo):
  skill install [--for claude,codex] [--scope user|project] [--force]   user: ~/.claude/skills, ~/.agents/skills
  skill uninstall [--for …] [--scope …] · skill show                   project: the instance's .claude/ and .agents/

  new instance <dir> --name "Acme Swarm" [--slug acme] [--org Acme] [--template empty|example] [--no-git]
  new agent < spec.json         create agents/<id>/ from a JSON spec (docs/api.md); --options: the choices and defaults
                                         create an instance (empty: stormo.yaml and the group unit;
                                         example: the Acme demo under the new name); git init unless --no-git
  instance                               the instance in use: name, slug, directory
  version                                print the engine version

  --json                                 machine output, one JSON event per line (docs/api.md)
`

var (
	fs           = pflag.NewFlagSet("stormo", pflag.ContinueOnError)
	fInstance    = fs.String("instance", "", "instance directory")
	fStore       = fs.String("store", "", "")
	fStatus      = fs.String("status", "", "")
	fScope       = fs.String("scope", "", "")
	fText        = fs.String("text", "", "")
	fRunner      = fs.String("runner", "mock", "")
	fEnvFile     = fs.String("env-file", "", "")
	fTag         = fs.String("tag", "", "")
	fSidecarTag  = fs.String("sidecar-tag", "", "")
	fLoop        = fs.Bool("loop", false, "")
	fDir         = fs.String("dir", "", "")
	fDev         = fs.Bool("dev", false, "")
	fYes         = fs.Bool("yes", false, "")
	fKeepHome    = fs.Bool("keep-home", false, "")
	fNapInterval = fs.Int("nap-interval", 0, "")
	fAllowProd   = fs.Bool("allow-prod-token", false, "")
	fRebuild     = fs.Bool("rebuild-sidecar", false, "")
	fSession     = fs.String("session", "swarm-local", "")
	fFollow      = fs.BoolP("follow", "f", false, "")
	fRemote      = fs.BoolP("remote", "r", false, "")
	fTarget      = fs.String("target", "", "")
	fTo          = fs.String("to", "", "")
	fFrom        = fs.String("from", "", "")
	fSince       = fs.String("since", "", "")
	fForce       = fs.Bool("force", false, "")
	fFor         = fs.String("for", "claude,codex", "")
	fHelp        = fs.BoolP("help", "h", false, "")
	fJSON        = fs.Bool("json", false, "")
	fNoOpen      = fs.Bool("no-open", false, "")
	fName        = fs.String("name", "", "")
	fOrg         = fs.String("org", "", "")
	fSlug        = fs.String("slug", "", "")
	fTemplate    = fs.String("template", "", "")
	fNoGit       = fs.Bool("no-git", false, "")
	fIfHash      = fs.String("if-hash", "", "")
	fData        = fs.Bool("data", false, "")
	fOut         = fs.StringP("out", "o", "", "")
	fAs          = fs.String("as", "", "")
	fUnit        = fs.String("unit", "", "")
	fReplace     = fs.Bool("replace", false, "")
	fWithActions = fs.Bool("with-actions", false, "")
	fKind        = fs.String("kind", "", "")
	fConnection  = fs.String("connection", "", "")
	fAgent       = fs.String("agent", "", "")
	fBaseURL     = fs.String("base-url", "", "")
	fKey         = fs.String("key", "", "")
	fOptions     = fs.Bool("options", false, "")
	errUsage     = errors.New("usage")
)

func printUsage() {
	name, root := "Stormo", "none found"
	if inst, err := instance.Current(); err == nil {
		name, root = inst.Name, inst.Root
	}
	fmt.Printf(usage, name, root, coreUsage())
}

// lifecycleResult is start, stop, restart and nap-now's result: the agents it went through, in order.
type lifecycleResult struct {
	Action string   `json:"action"`
	Where  string   `json:"where"`
	Agents []string `json:"agents"`
}

func need(v, what string) (string, error) {
	if v == "" {
		return "", withCode("usage", fmt.Errorf("missing %s (stormo --help)", what))
	}
	return v, nil
}

func main() {
	fs.Usage = func() {}
	if err := fs.Parse(os.Args[1:]); err != nil {
		if jsonMode() {
			emitError(withCode("usage", err))
		} else {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(2)
	}
	if jsonMode() {
		// Events own stdout; anything else this process or its children print (compose, docker,
		// text logs) goes to stderr, so the event stream stays one JSON object per line.
		os.Stdout = os.Stderr
	}
	if err := run(fs.Args()); err != nil {
		if jsonMode() {
			emitError(err)
			if errorCode(err) == "usage" {
				os.Exit(2)
			}
			os.Exit(1)
		}
		if errors.Is(err, errUsage) {
			printUsage()
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd, sub, rest := "", "", []string{}
	if len(args) > 0 {
		cmd = args[0]
	}
	if len(args) > 1 {
		sub, rest = args[1], args[2:]
	}
	if *fHelp || cmd == "help" {
		printUsage()
		return nil
	}
	if cmd == "version" {
		result(map[string]any{"version": version.String(), "api": version.API, "released": version.Released(), "exe": version.Exe()},
			func(w io.Writer) { fmt.Fprintln(w, version.String()) })
		return nil
	}
	if cmd == "skill" {
		return skillCmd(sub)
	}
	if cmd == "new" && sub == "instance" {
		// Before any instance is looked for: it makes one.
		dir, err := need(strings.Join(rest, " "), "directory")
		if err != nil {
			return err
		}
		r, err := scaffold.NewInstance(dir, scaffold.Options{Name: *fName, Org: *fOrg, Slug: *fSlug, Template: *fTemplate, Git: !*fNoGit}, stormo.Example())
		if err != nil {
			return withCode("usage", err)
		}
		result(r, func(w io.Writer) {
			fmt.Fprintf(w, "%s (%s) created at %s from the %s template", r.Name, r.Slug, r.Root, r.Template)
			if r.Git {
				fmt.Fprint(w, ", git initialised")
			}
			fmt.Fprintf(w, "\nnext: cd %q && stormo secrets init && stormo check\n", r.Root)
		})
		return nil
	}
	inst, err := instance.Current()
	if err != nil {
		return withCode("instance", err)
	}
	if cmd == "new" && sub == "agent" {
		if *fOptions {
			o := scaffold.NewAgentOptions(inst)
			result(o, func(w io.Writer) {
				d, unit := o.Defaults, o.Defaults.Unit
				if unit == "" {
					unit = "none (create one)"
				}
				fmt.Fprintf(w, "unit %s, engine %s %s, model %s via %s\n", unit, d.Engine.Kind, d.Engine.Version, d.Model.Name, d.Model.Provider)
			})
			return nil
		}
		var spec scaffold.AgentSpec
		if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&spec); err != nil {
			return withCode("usage", fmt.Errorf("new agent: a JSON spec on stdin (docs/api.md): %w", err))
		}
		r, err := scaffold.NewAgent(inst, spec)
		if err != nil {
			return withCode("invalid", err)
		}
		result(r, func(w io.Writer) {
			fmt.Fprintf(w, "%s (%s) created in unit %s: %s\nnext: stormo secrets set %s NAME for %s; stormo check %s\n",
				r.Name, r.ID, r.Unit, strings.Join(r.Files, ", "), r.ID, strings.Join(r.Secrets, ", "), r.ID)
		})
		return nil
	}
	if cmd == "instance" {
		result(map[string]string{"root": inst.Root, "name": inst.Name, "org": inst.Org, "slug": inst.Slug},
			func(w io.Writer) { fmt.Fprintf(w, "%s (%s)\n%s\n", inst.Name, inst.Slug, inst.Root) })
		return nil
	}
	where, err := ops.PickWhere(*fRemote, *fTarget, os.Getenv)
	if err != nil {
		return err
	}
	deps := ops.DefaultDeps(inst, *fYes)
	deps.Log = output().Line
	deps.Up = local.UpOptions{KeepHome: *fKeepHome, AllowProdToken: *fAllowProd, NapInterval: *fNapInterval, RebuildSidecar: *fRebuild}
	targets := func() []string {
		if sub == "" {
			return manifest.AgentIDs(inst.Root)
		}
		return append([]string{sub}, rest...)
	}
	loadAgent := func(id string) (*manifest.Agent, error) { return manifest.Load(inst.Root, id, inst.Names.Secret) }

	switch cmd {
	case "", "status", "ps":
		w := where
		if cmd == "ps" {
			w = place.Local
		}
		var ids []string
		if sub != "" {
			ids = targets()
		}
		rows, err := ops.AgentStatus(w, deps, ids)
		if err != nil {
			return err
		}
		result(rows, func(o io.Writer) { fmt.Fprintln(o, ops.RenderStatus(rows, time.Now())) })
		return nil

	case "review":
		var ids []string
		if sub != "" {
			ids = targets()
		}
		md, path, err := review.Run(inst, ids, *fSince)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n(saved %s; raw JSON next to it)\n", md, path)
		return nil

	case "check":
		rows, err := check(inst, targets())
		if err != nil {
			return err
		}
		result(rows, func(io.Writer) {})
		return nil

	case "config":
		return configCmd(inst, sub, rest)

	case "connections":
		switch sub {
		case "models":
			name, err := need(strings.Join(rest, " "), "connection name")
			if err != nil {
				return err
			}
			list, err := connectionModels(inst, name, *fAgent)
			if err != nil {
				return withCode("unavailable", err)
			}
			result(list, func(w io.Writer) {
				for _, m := range list {
					fmt.Fprintln(w, m.ID)
				}
			})
			return nil
		case "kinds":
			result(instance.ConnectionKinds, func(w io.Writer) {
				for _, k := range instance.ConnectionKinds {
					fmt.Fprintf(w, "%-11s %s\n", k.Kind, k.Label)
				}
			})
			return nil
		case "", "list":
			rows, err := connectionRows(inst)
			if err != nil {
				return err
			}
			result(rows, func(w io.Writer) {
				for _, r := range rows {
					fmt.Fprintf(w, "%-14s %-11s %-22s used by %s\n", r.Name, r.Kind, r.Key, strings.Join(r.UsedBy, ", "))
				}
			})
			return nil
		case "add":
			name, err := need(strings.Join(rest, " "), "connection name")
			if err != nil {
				return err
			}
			if err := instance.AddConnection(inst.Root, instance.Connection{Name: name, Kind: *fKind, BaseURL: *fBaseURL, Key: *fKey}); err != nil {
				return withCode("invalid", err)
			}
		case "remove":
			name, err := need(strings.Join(rest, " "), "connection name")
			if err != nil {
				return err
			}
			rows, err := connectionRows(inst)
			if err != nil {
				return err
			}
			for _, r := range rows {
				if r.Name == name && len(r.UsedBy) > 0 {
					return withCode("in_use", fmt.Errorf("%s is used by %s; point them elsewhere first", name, strings.Join(r.UsedBy, ", ")))
				}
			}
			if err := instance.RemoveConnection(inst.Root, name); err != nil {
				return withCode("invalid", err)
			}
		default:
			return errUsage
		}
		next, err := instance.Load(inst.Root)
		if err != nil {
			return err
		}
		rows, err := connectionRows(next)
		if err != nil {
			return err
		}
		result(rows, func(w io.Writer) { fmt.Fprintf(w, "%s %s\n", sub, strings.Join(rest, " ")) })
		return nil

	case "export":
		id, err := need(sub, "agent")
		if err != nil {
			return err
		}
		r, err := agentpack.Export(inst, id, *fData, *fOut)
		if err != nil {
			var pe *agentpack.Error
			if errors.As(err, &pe) {
				return withCode(pe.Code, err)
			}
			return err
		}
		if r.ContainsSecrets {
			step("WARNING: %s contains %s's secret values in plain text and its naps (client data). Keep it private, delete it once imported, rotate the values if it leaks.", r.Path, id)
		}
		result(r, func(w io.Writer) { fmt.Fprintf(w, "exported %s (%s, %d files) to %s\n", id, r.Mode, r.Files, r.Path) })
		return nil

	case "import":
		zipPath, err := need(sub, "zip file")
		if err != nil {
			return err
		}
		r, err := agentpack.Import(inst, zipPath, agentpack.ImportOptions{As: *fAs, Unit: *fUnit, Replace: *fReplace, WithActions: *fWithActions},
			func(id string) error { _, err := check(inst, []string{id}); return err })
		if err != nil {
			var pe *agentpack.Error
			if errors.As(err, &pe) {
				return withCode(pe.Code, err)
			}
			return err
		}
		for _, c := range r.Changes {
			step("%s: %s", r.Agent, c)
		}
		result(r, func(w io.Writer) { fmt.Fprintf(w, "imported %s from %s (%s)\n", r.Agent, r.From, r.Mode) })
		return nil

	case "migrate":
		if sub != "agent" {
			return errUsage
		}
		ids := rest
		if len(ids) == 0 {
			ids = manifest.AgentIDs(inst.Root)
		}
		compose, composeErr := local.ComposeCommand()
		reports := []*config.MigrateReport{}
		for _, id := range ids {
			running := false
			if composeErr == nil {
				st := place.LocalStateOf(id, func() ([]string, error) { return compose, nil })
				running = st.State == "running" || st.State == "starting" || st.State == "restarting"
			}
			r, err := config.MigrateAgent(inst, id)
			if err != nil {
				return err
			}
			if running {
				r.Data = "running"
			} else {
				changes, moved, err := ops.MigrateAgentData(inst, id)
				if err != nil {
					return err
				}
				r.Changes, r.Data = append(r.Changes, changes...), "in-place"
				if moved {
					r.Data = "moved"
				}
			}
			for _, c := range r.Changes {
				step("%s: %s", id, c)
			}
			if r.Data == "running" {
				step("%s: running, so its data stays where it is; `stormo stop %s && stormo migrate agent %s` moves it", id, id, id)
			}
			reports = append(reports, r)
		}
		if _, err := check(inst, ids); err != nil {
			return fmt.Errorf("migrated, but check fails: %w", err)
		}
		result(reports, func(io.Writer) {})
		return nil

	case "build":
		id, err := need(sub, "agent")
		if err != nil {
			return err
		}
		r, err := build.Agent(inst, id, build.Options{})
		if err != nil {
			return err
		}
		fmt.Printf("built %s (%d files, git %s)\n", r.Out, len(r.Info.Files), r.Info.GitSha)
		return nil

	case "inspect":
		id, err := need(sub, "agent")
		if err != nil {
			return err
		}
		t := manifest.AWS
		if *fTarget == "local" {
			t = manifest.Local
		}
		r, err := inspect.Agent(inst, id, t, nil)
		if err != nil {
			return err
		}
		body, err := learning.MarshalIndent(r)
		if err != nil {
			return err
		}
		fmt.Println(string(body))
		return nil

	case "bench":
		return runBench(inst, sub)

	case "deploy":
		return deployCmd(inst, sub, rest)

	case "shared":
		dir := *fDir
		if dir == "" {
			dir = shared.LocalDir(inst.Root)
			moved, conflicts, err := shared.MigrateLocal(inst.Root)
			if err != nil {
				return err
			}
			for _, l := range moved {
				fmt.Printf("moved .swarm/shared/%s to workdir/%s\n", l, l)
			}
			for _, l := range conflicts {
				fmt.Printf("WARNING: .swarm/shared/%s and workdir/%s both exist; merge the old one by hand\n", l, l)
			}
		}
		units := manifest.UnitIDs(inst.Root)
		for _, u := range units {
			if err := os.MkdirAll(filepath.Join(dir, u), 0o755); err != nil {
				return err
			}
		}
		fmt.Printf("local shared space at %s (%s)\n", dir, strings.Join(units, ", "))
		return nil

	case "start", "up":
		w := where
		if cmd == "up" {
			w = place.Local
		}
		ids := targets()
		for _, id := range ids {
			if err := ops.Start(w, id, deps); err != nil {
				return err
			}
		}
		result(lifecycleResult{"start", string(w), ids}, nil)
		return nil

	case "stop", "down":
		w := where
		if cmd == "down" {
			w = place.Local
		}
		ids := targets()
		for _, id := range ids {
			if err := ops.Stop(w, id, deps); err != nil {
				return err
			}
		}
		result(lifecycleResult{"stop", string(w), ids}, nil)
		return nil

	case "restart":
		ids := targets()
		for _, id := range ids {
			if err := ops.Restart(where, id, deps); err != nil {
				return err
			}
		}
		result(lifecycleResult{"restart", string(where), ids}, nil)
		return nil

	case "handoff":
		id, err := need(sub, "agent")
		if err != nil {
			return err
		}
		if *fTo != "local" && *fTo != "remote" {
			return fmt.Errorf(`--to must be local or remote, got "%s"`, *fTo)
		}
		return ops.Handoff(id, place.Where(*fTo), deps, ops.HandoffOptions{Force: *fForce})

	case "logs":
		id, err := need(sub, "agent")
		if err != nil {
			return err
		}
		return local.Logs(inst, id, *fFollow)

	case "nap-now":
		id, err := need(sub, "agent")
		if err != nil {
			return err
		}
		if err := local.NapNow(inst, id); err != nil {
			return err
		}
		result(lifecycleResult{"nap-now", string(place.Local), []string{id}}, nil)
		return nil

	case "core":
		return runCore(inst, sub, rest)

	case "chat":
		id, err := need(sub, "agent")
		if err != nil {
			return err
		}
		msg, err := need(strings.Join(rest, " "), "message")
		if err != nil {
			return err
		}
		reply, err := local.Chat(inst, id, msg, *fSession)
		if err != nil {
			return err
		}
		fmt.Println(reply)
		return nil

	case "slack":
		if sub != "manifest" || len(rest) == 0 {
			return errUsage
		}
		r, err := slack.Manifest(inst, rest[0], *fDev, slack.Exec)
		if err != nil {
			return err
		}
		fmt.Printf("wrote %s\n\n%s\n", r.Path, strings.Join(r.Steps, "\n"))
		return nil

	case "secrets":
		return secretsCmd(inst, deps, sub, rest)

	case "dream":
		id, err := need(sub, "agent")
		if err != nil {
			return err
		}
		uri := *fStore
		if uri == "" {
			uri = os.Getenv("SWARM_STORE")
		}
		var st loop.Store
		if uri == "" {
			st = loop.LocalStore{Root: inst.Root}
		} else if st, err = loop.Open(uri, region(inst)); err != nil {
			return err
		}
		r, err := loop.Dream(inst, id, st)
		if err != nil {
			return err
		}
		if len(r.Naps) == 0 {
			fmt.Printf("no new naps for %s\n", id)
			return nil
		}
		cron := ""
		if r.CronProposal {
			cron = ", cron changed"
		}
		fmt.Printf("folded %d naps: %d new learnings, %d re-sighted, %d skill proposals%s. See agents/%s/learnings/DREAM.md\n", len(r.Naps), r.NewLearnings, r.Resighted, len(r.SkillProposals), cron, id)
		return nil

	case "learn":
		return learnCmd(inst, deps, where, sub, rest, targets)

	case "rehydrate":
		sc, err := sidecarEnv(inst)
		if err != nil {
			return err
		}
		a, err := loadAgent(sc.agent)
		if err != nil {
			return err
		}
		eng, err := engines.Get(a.Engine.Kind, inst)
		if err != nil {
			return err
		}
		rep, err := loop.Rehydrate(loop.RehydrateOptions{Agent: a, Engine: eng, Home: sc.home, BaselineDir: sc.baselineDir, Store: sc.store, Scrubber: learning.NewScrubber(inst)})
		if err != nil {
			return err
		}
		body, _ := learning.MarshalCompact(rep)
		fmt.Printf("[rehydrate] %s\n", body)
		return nil

	case "nap":
		sc, err := sidecarEnv(inst)
		if err != nil {
			return err
		}
		a, err := loadAgent(sc.agent)
		if err != nil {
			return err
		}
		eng, err := engines.Get(a.Engine.Kind, inst)
		if err != nil {
			return err
		}
		o := loop.NapOptions{Agent: a.ID, Unit: a.Unit, Engine: eng, Home: sc.home, Store: sc.store, Instance: instanceID(), BaselineDir: sc.baselineDir}
		if *fLoop {
			interval := a.Learning.NapIntervalSeconds
			if v := os.Getenv("SWARM_NAP_INTERVAL"); v != "" {
				fmt.Sscan(v, &interval)
			}
			loop.NapLoop(o, time.Duration(interval)*time.Second, 90*time.Second)
			return nil
		}
		o.Reason = "manual"
		n, err := loop.NapOnce(o)
		if err != nil {
			return err
		}
		if n == nil {
			fmt.Println("[nap] unchanged")
		} else {
			fmt.Printf("[nap] %s (%d files)\n", n.ID, len(n.Files))
		}
		return nil
	}
	fmt.Fprintf(os.Stderr, "unknown command %s\n\n", cmd)
	return errUsage
}

func region(inst *instance.Instance) string {
	if inst.Aws.Region == instance.Unset {
		return ""
	}
	return inst.Aws.Region
}

// checked is one agent that passed check.
type checked struct {
	Agent  string `json:"agent"`
	Unit   string `json:"unit"`
	Engine string `json:"engine"`
	Files  int    `json:"files"`
	Skills int    `json:"skills"`
	// The manifest's agent.yaml format and the format-0 keys it still uses.
	Format int      `json:"format"`
	Legacy []string `json:"legacy"`
}

// check validates agents (manifest, bridge actions, a full build, Slack ownership, the secrets
// files) and reports each as a step; the rows are the check command's result.
func check(inst *instance.Instance, ids []string) ([]checked, error) {
	registry, err := bridge.Load(inst)
	if err != nil {
		return nil, err
	}
	rows := []checked{}
	for _, id := range ids {
		a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			return nil, err
		}
		if _, err := bridge.ActionsFor(a, registry); err != nil {
			return nil, err
		}
		r, err := build.Agent(inst, id, build.Options{Out: filepath.Join(inst.Root, ".swarm", "check", id)})
		if err != nil {
			return nil, err
		}
		rows = append(rows, checked{id, a.Unit, a.Engine.Kind, len(r.Info.Files), len(r.Info.Skills), a.Format, orNone(a.Legacy)})
		step("ok  %s  unit=%s engine=%s files=%d skills=%d", id, a.Unit, a.Engine.Kind, len(r.Info.Files), len(r.Info.Skills))
		if len(a.Legacy) > 0 {
			step("    %s uses agent.yaml format 0 (%s); `stormo migrate agent %s` moves it to format %d", id, strings.Join(a.Legacy, ", "), id, manifest.Format)
		}
	}
	owners, err := slack.SlashCommandOwners(inst)
	if err != nil {
		return nil, err
	}
	if len(owners) > 1 {
		return nil, fmt.Errorf("SLACK: slash_commands is set for %s; only one agent's app may own Slack's command names", strings.Join(owners, " and "))
	}
	if problems := secrets.FileProblems(inst.Root); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "SECRETS: "+p)
		}
		return nil, errors.New("secrets file problems")
	}
	return rows, nil
}

// configCmd shows and replaces configuration files for editors (pkg/config, docs/api.md).
func configCmd(inst *instance.Instance, sub string, rest []string) error {
	file, err := need(strings.Join(rest, " "), "file (agents/<id>/agent.yaml or agents/<id>/SOUL.md)")
	if err != nil {
		return err
	}
	var f *config.File
	switch sub {
	case "show":
		f, err = config.Show(inst, file)
	case "write", "apply":
		var body []byte
		if body, err = io.ReadAll(os.Stdin); err == nil && sub == "write" {
			f, err = config.Write(inst, file, body, *fIfHash)
		} else if err == nil {
			f, err = config.Apply(inst, file, body, *fIfHash)
		}
	default:
		return errUsage
	}
	var ce *config.Error
	if errors.As(err, &ce) {
		return withCode(ce.Code, err)
	}
	if err != nil {
		return err
	}
	result(f, func(w io.Writer) {
		if sub == "show" {
			fmt.Fprint(w, f.Text)
			return
		}
		fmt.Fprintf(w, "wrote %s (%s)\n", f.Path, f.Hash)
	})
	return nil
}

func runBench(inst *instance.Instance, sub string) error {
	id, err := need(sub, "agent")
	if err != nil {
		return err
	}
	a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		return err
	}
	eng, err := engines.Get(a.Engine.Kind, inst)
	if err != nil {
		return err
	}
	scenarios, err := bench.LoadScenarios(inst.Root, id)
	if err != nil {
		return err
	}
	if *fTag != "" {
		scenarios = slices.DeleteFunc(scenarios, func(s bench.Scenario) bool { return !slices.Contains(s.Tags, *fTag) })
	}
	runner := bench.Runner(bench.MockRunner)
	if *fRunner == "docker" {
		r, err := build.Agent(inst, id, build.Options{})
		if err != nil {
			return err
		}
		envFile := *fEnvFile
		if envFile == "" {
			if envFile, _, err = secrets.WriteLocalEnv(inst.Root, a); err != nil {
				return err
			}
		}
		dir := *fDir
		if dir == "" {
			dir = shared.LocalDir(inst.Root)
		}
		runner = bench.DockerRunner(a, eng, r.Out, envFile, dir)
	}
	results, err := bench.Run(scenarios, runner)
	if err != nil {
		return err
	}
	failed := 0
	for _, r := range results {
		if r.OK {
			fmt.Printf("PASS  %s\n", r.ID)
		} else {
			failed++
			fmt.Printf("FAIL  %s\n      %s\n", r.ID, strings.Join(r.Failures, "\n      "))
		}
	}
	fmt.Printf("\n%d/%d passed (%s runner)\n", len(results)-failed, len(results), *fRunner)
	if failed > 0 {
		return fmt.Errorf("%d bench scenarios failed", failed)
	}
	return nil
}

func deployCmd(inst *instance.Instance, sub string, rest []string) error {
	t := deploy.DefaultTarget(inst)
	if sub == "render-shared" {
		fmt.Println(strings.Join(deploy.SharedAccessPointCommands(manifest.UnitIDs(inst.Root), t), "\n"))
		return nil
	}
	if sub != "render" || len(rest) == 0 {
		return errUsage
	}
	id := rest[0]
	// As it will run on AWS: optional secrets absent from the aws layer are left out, so the task
	// definition never points ECS at a Secrets Manager key that does not exist.
	declared, err := manifest.Load(inst.Root, id, inst.Names.Secret)
	if err != nil {
		return err
	}
	present, err := secrets.Present(inst.Root, declared, manifest.AWS)
	if err != nil {
		return err
	}
	eff := manifest.ApplyOptional(declared, present)
	for _, x := range eff.Off {
		fmt.Fprintf(os.Stderr, "%s: %s has no aws value, so off on ECS: %s\n", id, x.Name, strings.Join(x.What, ", "))
	}
	if *fSidecarTag != "" {
		t.SidecarTag = *fSidecarTag
	}
	eng, err := engines.Get(eff.Agent.Engine.Kind, inst)
	if err != nil {
		return err
	}
	dir := filepath.Join(inst.Root, "dist", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	td, _ := learning.MarshalIndent(deploy.RenderTaskDef(eff.Agent, eng, t))
	pol, _ := learning.MarshalIndent(deploy.RenderTaskPolicy(eff.Agent, t))
	if err := os.WriteFile(filepath.Join(dir, "taskdef.json"), td, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "task-policy.json"), pol, 0o644); err != nil {
		return err
	}
	if missing := deploy.Unresolved(eff.Agent, t); len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: placeholders left (%s); run `stormo deploy render-shared` first.\n\n", strings.Join(missing, ", "))
	}
	fmt.Printf("wrote dist/%s/taskdef.json and task-policy.json. To apply (not run by this tool):\n\n", id)
	fmt.Println(strings.Join(deploy.ApplyCommands(eff.Agent, t, ""), "\n"))
	return nil
}

func secretsCmd(inst *instance.Instance, deps *ops.Deps, sub string, rest []string) error {
	ids := rest
	if len(ids) == 0 {
		ids = manifest.AgentIDs(inst.Root)
	}
	path := secrets.Path(inst.Root)
	switch sub {
	case "set":
		// The value comes on stdin, never in argv (process lists, shell history).
		if len(rest) != 2 {
			return withCode("usage", errors.New("usage: stormo secrets set <shared|agent> NAME (value on stdin)"))
		}
		body, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
		if err != nil {
			return err
		}
		if err := secrets.Set(inst, rest[0], rest[1], strings.TrimRight(string(body), "\r\n")); err != nil {
			return withCode("invalid", err)
		}
		result(map[string]string{"scope": rest[0], "name": rest[1]}, func(w io.Writer) { fmt.Fprintf(w, "%s set for %s\n", rest[1], rest[0]) })
		return nil
	case "init":
		p, added, err := secrets.Init(inst)
		if err != nil {
			return err
		}
		msg := "nothing new"
		if len(added) > 0 {
			msg = "added " + strings.Join(added, ", ")
		}
		fmt.Printf("%s: %s (fill in empty values)\n", p, msg)
		return nil
	case "share":
		if len(rest) == 0 {
			return errors.New("usage: stormo secrets share NAME... [--from <agent>]")
		}
		f, err := secrets.Load(path)
		if err != nil {
			return err
		}
		r, err := secrets.Share(f, rest, *fFrom)
		if err != nil {
			return err
		}
		if len(r.Shared) > 0 {
			if err := secrets.Save(path, inst.Names.Resource, f); err != nil {
				return err
			}
		}
		orNone := func(l []string) string {
			if len(l) == 0 {
				return "none"
			}
			return strings.Join(l, ", ")
		}
		fmt.Println("shared: " + orNone(r.Shared))
		if len(r.Removed) > 0 {
			fmt.Println("dropped per-agent copies: " + strings.Join(r.Removed, ", "))
		}
		if len(r.Kept) > 0 {
			fmt.Println("kept (different value, still overrides shared): " + strings.Join(r.Kept, ", "))
		}
		if len(r.Missing) > 0 {
			hint := " (pass --from <agent>)"
			if *fFrom != "" {
				hint = " (not set for " + *fFrom + ")"
			}
			fmt.Println("no value to share: " + strings.Join(r.Missing, ", ") + hint)
		}
		return nil
	case "check":
		f, err := secrets.Load(path)
		if err != nil {
			return err
		}
		problems := secrets.FileProblems(inst.Root)
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "!! "+p)
		}
		bad := len(problems) > 0
		for _, id := range ids {
			a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
			if err != nil {
				return err
			}
			for _, t := range []manifest.Target{manifest.AWS, manifest.Local} {
				r := secrets.Resolve(f, a, t)
				n := len(secrets.Names(a, t))
				status := "complete"
				if len(r.Missing) > 0 {
					status = "missing " + strings.Join(r.Missing, ", ")
					bad = true
				}
				line := fmt.Sprintf("%-12s %-5s %d/%d %s", id, t, n-len(r.Missing), n, status)
				if len(r.Off) > 0 {
					line += "  optional off: " + strings.Join(r.Off, ", ")
				}
				if len(r.Overridden) > 0 {
					line += "  local overrides: " + strings.Join(r.Overridden, ", ")
				}
				fmt.Println(line)
			}
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			fmt.Printf("(no %s; run `stormo secrets init`)\n", path)
		}
		if bad {
			return errors.New("secrets incomplete")
		}
		return nil
	case "env":
		if len(rest) == 0 {
			return errUsage
		}
		a, err := manifest.Load(inst.Root, rest[0], inst.Names.Secret)
		if err != nil {
			return err
		}
		p, r, err := secrets.WriteLocalEnv(inst.Root, a)
		if err != nil {
			return err
		}
		missing := ""
		if len(r.Missing) > 0 {
			missing = "; missing " + strings.Join(r.Missing, ", ")
		}
		fmt.Printf("wrote %s (%d values%s)\n", p, r.Values.Len(), missing)
		return nil
	case "push":
		f, err := secrets.Load(path)
		if err != nil {
			return err
		}
		if problems := secrets.FileProblems(inst.Root); len(problems) > 0 {
			return errors.New(strings.Join(problems, "\n"))
		}
		for _, id := range ids {
			a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
			if err != nil {
				return err
			}
			r := secrets.Resolve(f, a, manifest.AWS)
			plan, err := secrets.PlanPush(a, r, deps.Target.Region, deps.AWS)
			if err != nil {
				return err
			}
			state := "will be created"
			if plan.Exists {
				state = "exists"
			}
			out := fmt.Sprintf("%s → %s (%s)\n", id, plan.SecretID, state)
			for _, x := range []struct {
				label string
				keys  []string
			}{{"add", plan.Added}, {"change", plan.Changed}, {"remove (not declared)", plan.Removed}, {"unchanged", plan.Unchanged}, {"MISSING locally", plan.Missing}} {
				if len(x.keys) > 0 {
					out += "  " + x.label + ": " + strings.Join(x.keys, ", ") + "\n"
				}
			}
			fmt.Println(out)
			if len(plan.Missing) > 0 {
				fmt.Println("  skipped: fill the missing values first")
				continue
			}
			if len(plan.Added)+len(plan.Changed)+len(plan.Removed) == 0 {
				continue
			}
			if !*fYes {
				fmt.Println("  dry run; re-run with --yes to write")
				continue
			}
			if err := secrets.ApplyPush(plan, r, deps.Target.Region, inst.Names.Resource, deps.AWS); err != nil {
				return err
			}
			fmt.Println("  written")
		}
		return nil
	}
	return errUsage
}

var reviewVerbs = []string{"list", "accept", "reject", "pin", "unpin", "edit", "promote", "skills", "skill-accept", "skill-reject"}

func learnCmd(inst *instance.Instance, deps *ops.Deps, where place.Where, sub string, rest []string, targets func() []string) error {
	if !slices.Contains(reviewVerbs, sub) {
		for _, id := range targets() {
			if _, err := ops.Learn(where, id, deps); err != nil {
				return err
			}
		}
		return nil
	}
	if len(rest) == 0 {
		return errUsage
	}
	id, ids := rest[0], rest[1:]
	root := inst.Root
	switch sub {
	case "list":
		ledger, err := learning.LoadLedger(root, id)
		if err != nil {
			return err
		}
		n := 0
		for _, e := range ledger {
			if *fStatus != "" && string(e.Status) != *fStatus {
				continue
			}
			n++
			pinned, pii := "", ""
			if e.IsPinned() {
				pinned = " pinned"
			}
			if len(e.PII) > 0 {
				pii = " pii=" + strings.Join(e.PII, ",")
			}
			text := []rune(strings.ReplaceAll(e.Text, "\n", " "))
			if len(text) > 200 {
				text = text[:200]
			}
			fmt.Printf("%s  %-8s %-6s %-5s seen=%d%s%s\n    %s\n", e.ID, e.Status, e.Kind, e.Scope, e.SeenCount, pinned, pii, string(text))
		}
		if n == 0 {
			fmt.Println("(empty)")
		}
		return nil
	case "accept", "reject":
		status := learning.Accepted
		if sub == "reject" {
			status = learning.Rejected
		}
		touched, err := loop.Decide(root, id, ids, loop.Change{Status: &status})
		for _, e := range touched {
			fmt.Printf("%s %s\n", status, e.ID)
		}
		return err
	case "pin", "unpin":
		pin := sub == "pin"
		touched, err := loop.Decide(root, id, ids, loop.Change{Pinned: &pin})
		for _, e := range touched {
			fmt.Printf("%sned %s\n", sub, e.ID)
		}
		return err
	case "edit":
		text, err := need(*fText, "--text")
		if err != nil || len(ids) == 0 {
			return errUsage
		}
		if _, err := loop.Decide(root, id, ids[:1], loop.Change{Text: &text}); err != nil {
			return err
		}
		fmt.Printf("edited %s\n", ids[0])
		return nil
	case "promote":
		scope, err := need(*fScope, "--scope")
		if err != nil {
			return err
		}
		s := manifest.Scope(scope)
		touched, err := loop.Decide(root, id, ids, loop.Change{Scope: &s})
		for _, e := range touched {
			fmt.Printf("promoted %s → %s\n", e.ID, e.Scope)
		}
		return err
	case "skills":
		props, err := loop.ListSkillProposals(root, id)
		for _, p := range props {
			scrubbed := ""
			if len(p.PII) > 0 {
				scrubbed = "  scrubbed: " + strings.Join(p.PII, ",")
			}
			fmt.Printf("%s  from %s%s\n", p.Skill, p.Nap, scrubbed)
		}
		return err
	case "skill-accept", "skill-reject":
		if len(ids) == 0 {
			return errUsage
		}
		decision, verb := "accepted", "applied"
		if sub == "skill-reject" {
			decision, verb = "rejected", "rejected"
		}
		if _, err := loop.DecideSkill(root, id, ids[0], decision); err != nil {
			return err
		}
		fmt.Printf("%s %s\n", verb, ids[0])
		return nil
	}
	return errUsage
}

type sidecar struct {
	agent, home, baselineDir string
	store                    loop.Store
}

func sidecarEnv(inst *instance.Instance) (*sidecar, error) {
	agent, err := need(os.Getenv("SWARM_AGENT"), "SWARM_AGENT")
	if err != nil {
		return nil, err
	}
	uri, err := need(os.Getenv("SWARM_STORE"), "SWARM_STORE")
	if err != nil {
		return nil, err
	}
	st, err := loop.Open(uri, region(inst))
	if err != nil {
		return nil, err
	}
	home := os.Getenv("SWARM_HOME")
	if home == "" {
		home = "/data"
	}
	return &sidecar{agent: agent, home: home, baselineDir: filepath.Join(inst.Root, "dist", agent, "baseline"), store: st}, nil
}

var hostSafe = regexp.MustCompile(`[^a-z0-9-]`)

// instanceID names this task (ECS task id) or host, for nap ids.
func instanceID() string {
	if uri := os.Getenv("ECS_CONTAINER_METADATA_URI_V4"); uri != "" {
		client := http.Client{Timeout: 3 * time.Second}
		if res, err := client.Get(uri + "/task"); err == nil {
			var task struct{ TaskARN string }
			if json.NewDecoder(res.Body).Decode(&task) == nil && task.TaskARN != "" {
				res.Body.Close()
				id := task.TaskARN[strings.LastIndex(task.TaskARN, "/")+1:]
				return id[:min(12, len(id))]
			}
			res.Body.Close()
		}
	}
	h, _ := os.Hostname()
	h = hostSafe.ReplaceAllString(strings.ToLower(strings.SplitN(h, ".", 2)[0]), "")
	if h == "" {
		return "local"
	}
	return h[:min(24, len(h))]
}

// skillCmd installs, removes or prints the agent skill. It needs no instance for the user scope.
func skillCmd(sub string) error {
	commands := fmt.Sprintf(usage, "an instance's", "found from the working directory", coreUsage())
	files := skill.Files(version.String(), commands)
	if sub == "show" {
		fmt.Print(files["SKILL.md"])
		return nil
	}
	if sub != "install" && sub != "uninstall" {
		return errUsage
	}
	scope := skill.Scope(*fScope)
	if scope == "" {
		scope = skill.User
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	root := ""
	if scope == skill.Project {
		if root, err = projectRoot(); err != nil {
			return err
		}
	}
	for _, name := range strings.Split(*fFor, ",") {
		tool := skill.Tool(strings.TrimSpace(name))
		dir, err := skill.Dir(tool, scope, home, root)
		if err != nil {
			return err
		}
		if sub == "uninstall" {
			removed, err := skill.Uninstall(dir, *fForce)
			if err != nil {
				return err
			}
			if removed {
				fmt.Printf("%-6s removed %s\n", tool, dir)
			} else {
				fmt.Printf("%-6s nothing at %s\n", tool, dir)
			}
			continue
		}
		if err := skill.Install(dir, files, *fForce); err != nil {
			return err
		}
		fmt.Printf("%-6s installed %s\n", tool, dir)
	}
	if sub == "install" {
		fmt.Println("Claude Code and Codex pick it up in running sessions (in Claude Code, /reload-skills if its skills folder is new).")
	}
	return nil
}

// projectRoot is the instance found from the working directory (or --instance / STORMO_INSTANCE;
// never the one the binary happens to live in), else the git repository, else the directory itself.
func projectRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if root := instance.Find(cwd, os.Getenv, "", os.Args[1:]); root != "" {
		return root, nil
	}
	if out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output(); err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	return cwd, nil
}

func orNone(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// connectionRow is one connection with what uses it and whether its key has a value.
type connectionRow struct {
	instance.Connection
	Label string `json:"label"`
	API   bool   `json:"api"`
	// KeySet: the key has a shared value; AgentKeys: agents with their own value for it.
	KeySet    bool     `json:"keySet"`
	AgentKeys []string `json:"agentKeys"`
	UsedBy    []string `json:"usedBy"`
}

func connectionRows(inst *instance.Instance) ([]connectionRow, error) {
	f, err := secrets.Load(secrets.Path(inst.Root))
	if err != nil {
		return nil, err
	}
	agents := []*manifest.Agent{}
	for _, id := range manifest.AgentIDs(inst.Root) {
		if a, err := manifest.Load(inst.Root, id, inst.Names.Secret); err == nil {
			agents = append(agents, a)
		}
	}
	rows := []connectionRow{}
	for _, c := range inst.Connections {
		k, _ := instance.KindOf(c.Kind)
		r := connectionRow{Connection: c, Label: k.Label, API: k.API, AgentKeys: []string{}, UsedBy: []string{}}
		if c.Key != "" && f.Shared != nil {
			v, _ := f.Shared.Get(c.Key)
			r.KeySet = v != ""
		}
		for _, a := range agents {
			uses := a.Engine.Provider == c.Name || (a.Engine.Local != nil && a.Engine.Local.ConnectionName() == c.Name)
			for _, s := range a.Schedules {
				uses = uses || s.Provider == c.Name
			}
			if uses {
				r.UsedBy = append(r.UsedBy, a.ID)
			}
			if c.Key != "" {
				if l := f.Agents.Get(a.ID); l != nil {
					if v, _ := l.Get(c.Key); v != "" {
						r.AgentKeys = append(r.AgentKeys, a.ID)
					}
				}
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// connectionModels lists a connection's models: an API asks with its key (the agent's value when
// --agent is given, else the shared one); a ChatGPT sign-in's plan catalog comes from the core.
func connectionModels(inst *instance.Instance, name, agent string) ([]models.Model, error) {
	c, ok := inst.Connection(name)
	if !ok {
		return nil, fmt.Errorf("no connection named %s", name)
	}
	if !c.API() {
		client := &http.Client{Timeout: 20 * time.Second}
		if res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/gateway/models?connection=%s", core.CorePort(), name)); err == nil {
			var m struct{ Models []llm.ModelInfo }
			ok := res.StatusCode == http.StatusOK && json.NewDecoder(res.Body).Decode(&m) == nil
			res.Body.Close()
			if ok {
				out := []models.Model{}
				for _, x := range m.Models {
					out = append(out, models.Model{ID: x.ID, Name: x.DisplayName, ContextLength: x.ContextLength})
				}
				return out, nil
			}
		}
		// An older core: its status lists the default connection's models.
		res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/gateway", core.CorePort()))
		if err != nil {
			return nil, fmt.Errorf("the core is not running: start it to list %s's plan models", name)
		}
		defer res.Body.Close()
		var g struct {
			Models      []llm.ModelInfo `json:"models"`
			Connections []struct {
				Name   string          `json:"name"`
				Models []llm.ModelInfo `json:"models"`
			} `json:"connections"`
		}
		if err := json.NewDecoder(res.Body).Decode(&g); err != nil {
			return nil, err
		}
		list := g.Models
		if name != instance.DefaultChatGPT {
			list = nil
			for _, cs := range g.Connections {
				if cs.Name == name {
					list = cs.Models
				}
			}
			if list == nil {
				return nil, fmt.Errorf("the core does not serve %s yet: restart it", name)
			}
		}
		out := []models.Model{}
		for _, m := range list {
			out = append(out, models.Model{ID: m.ID, Name: m.DisplayName, ContextLength: m.ContextLength})
		}
		return out, nil
	}
	f, err := secrets.Load(secrets.Path(inst.Root))
	if err != nil {
		return nil, err
	}
	key := ""
	if f.Shared != nil {
		key, _ = f.Shared.Get(c.Key)
	}
	if agent != "" {
		if a, err := manifest.Load(inst.Root, agent, inst.Names.Secret); err == nil {
			if v, ok := secrets.Resolve(f, a, manifest.Local).Values.Get(c.Key); ok && v != "" {
				key = v
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return models.List(ctx, c, key)
}
