// Command stormo operates an instance's agents: build, run (locally or on ECS), deploy, bench, and
// the nap/dream learning loop. The same binary runs inside the sidecars (rehydrate, nap).
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/bench"
	"github.com/camfinc/stormo/pkg/bridge"
	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/deploy"
	"github.com/camfinc/stormo/pkg/engines"
	"github.com/camfinc/stormo/pkg/inspect"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/ops"
	"github.com/camfinc/stormo/pkg/place"
	"github.com/camfinc/stormo/pkg/review"
	"github.com/camfinc/stormo/pkg/secrets"
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
  build <agent>                          compile dist/<agent>/baseline (also a Hermes distribution)
  inspect <agent> [--target aws|local]   everything the engine computes for the agent, as JSON
  bench <agent> [--runner mock|docker] [--env-file f] [--tag t]   docker: env from secrets.local.yaml
  deploy render <agent> [--sidecar-tag t] write dist/<agent>/{taskdef,task-policy}.json, print aws commands
  deploy render-shared                   print the one-time EFS setup for the shared document space
  shared [--dir .swarm/shared]           create the local shared space used by the docker bench

  slack manifest <agent> [--dev]         generate the agent's Slack app manifest (+ setup steps)

  review [agent...] [--since 24h]        read-only fleet health sweep (.swarm/review/)
  secrets init                           create/extend secrets.local.yaml with every declared name
  secrets share NAME... [--from <agent>] keep one value under shared: and drop identical per-agent copies
  secrets check [agent...]               report missing values (aws and local) and file problems
  secrets env <agent>                    write .swarm/env/<agent>.env (local overlay applied)
  secrets push [agent...] [--yes]        diff key names vs AWS Secrets Manager; --yes writes

  dream <agent> [--store s3://bucket|dir] fold naps from an explicit store (learn does this for you)

%s
  sidecar (inside the task; env SWARM_AGENT, SWARM_STORE, SWARM_HOME):
  rehydrate                              build the engine home from baseline + latest nap
  nap [--loop]                           snapshot the engine home to the store

  agent skill (teaches Claude Code and Codex to operate stormo):
  skill install [--for claude,codex] [--scope user|project] [--force]   user: ~/.claude/skills, ~/.agents/skills
  skill uninstall [--for …] [--scope …] · skill show                   project: the instance's .claude/ and .agents/

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
	errUsage     = errors.New("usage")
)

func printUsage() {
	name, root := "Stormo", "none found"
	if inst, err := instance.Current(); err == nil {
		name, root = inst.Name, inst.Root
	}
	fmt.Printf(usage, name, root, coreUsage())
}

func need(v, what string) (string, error) {
	if v == "" {
		return "", fmt.Errorf("missing %s (stormo --help)", what)
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
	if err := run(fs.Args()); err != nil {
		if jsonMode() {
			emitError(err)
			if errors.Is(err, errUsage) {
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
			func() { fmt.Println(version.String()) })
		return nil
	}
	if cmd == "skill" {
		return skillCmd(sub)
	}
	inst, err := instance.Current()
	if err != nil {
		return withCode("instance", err)
	}
	if cmd == "instance" {
		result(map[string]string{"root": inst.Root, "name": inst.Name, "org": inst.Org, "slug": inst.Slug},
			func() { fmt.Printf("%s (%s)\n%s\n", inst.Name, inst.Slug, inst.Root) })
		return nil
	}
	where, err := ops.PickWhere(*fRemote, *fTarget, os.Getenv)
	if err != nil {
		return err
	}
	deps := ops.DefaultDeps(inst, *fYes)
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
		fmt.Println(ops.RenderStatus(rows, time.Now()))
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
		return check(inst, targets())

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
			dir = filepath.Join(inst.Root, ".swarm", "shared")
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
		for _, id := range targets() {
			if err := ops.Start(w, id, deps); err != nil {
				return err
			}
		}
		return nil

	case "stop", "down":
		w := where
		if cmd == "down" {
			w = place.Local
		}
		for _, id := range targets() {
			if err := ops.Stop(w, id, deps); err != nil {
				return err
			}
		}
		return nil

	case "restart":
		for _, id := range targets() {
			if err := ops.Restart(where, id, deps); err != nil {
				return err
			}
		}
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
		return local.NapNow(inst, id)

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
		if uri == "" {
			uri = filepath.Join(inst.Root, ".swarm", "store")
		}
		st, err := loop.Open(uri, region(inst))
		if err != nil {
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

func check(inst *instance.Instance, ids []string) error {
	registry, err := bridge.Load(inst)
	if err != nil {
		return err
	}
	for _, id := range ids {
		a, err := manifest.Load(inst.Root, id, inst.Names.Secret)
		if err != nil {
			return err
		}
		if _, err := bridge.ActionsFor(a, registry); err != nil {
			return err
		}
		r, err := build.Agent(inst, id, build.Options{Out: filepath.Join(inst.Root, ".swarm", "check", id)})
		if err != nil {
			return err
		}
		fmt.Printf("ok  %s  unit=%s engine=%s files=%d skills=%d\n", id, a.Unit, a.Engine.Kind, len(r.Info.Files), len(r.Info.Skills))
	}
	owners, err := slack.SlashCommandOwners(inst)
	if err != nil {
		return err
	}
	if len(owners) > 1 {
		return fmt.Errorf("SLACK: slash_commands is set for %s; only one agent's app may own Slack's command names", strings.Join(owners, " and "))
	}
	if problems := secrets.FileProblems(inst.Root); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "SECRETS: "+p)
		}
		return errors.New("secrets file problems")
	}
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
			dir = filepath.Join(inst.Root, ".swarm", "shared")
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
