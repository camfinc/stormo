// Package cli is the needletail binary's commands (docs/needletail.md §2): serve (the container's
// entrypoint), oneshot (the bench), healthcheck (the image's health check, so it needs no curl)
// and version.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/camfinc/stormo/internal/needletail/api"
	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/jail"
	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/luau"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/privsep"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/secrets"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/slack"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Version is set at build time (-ldflags -X).
var Version = "dev"

const usage = `usage: needletail <command>

  serve                                   run the agent: its API on $NEEDLETAIL_API_PORT (default 8642)
  oneshot -q <prompt> [--format stream-json|text]
                                          one turn in a new session, then exit (the bench)
  healthcheck                             exit 0 when the local API answers /health
  luau check <file>                       validate a .luau tool's, handler's or script's header and compile it
  luau reference                          print the Luau functions tools may call (markdown)
  version                                 print the version
`

// Shutdown budget: turns get this long to finish after SIGTERM before they are cancelled.
const drainTurns = 20 * time.Second

// Run runs one command; ctx ends serve (main cancels it on SIGTERM/SIGINT). It returns the exit
// code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	// `docker run <image> needletail oneshot …` (the bench's argv) reaches the entrypoint with the
	// binary's own name first.
	if len(args) > 0 && args[0] == "needletail" {
		args = args[1:]
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "jail":
		// not in the usage: how the runtime starts the agent's commands (jail.Launcher)
		return jail.Main(args[1:], stderr)
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "oneshot":
		return oneshot(ctx, args[1:], stdout, stderr)
	case "healthcheck":
		return healthcheck(stderr)
	case "luau":
		return luauCommand(ctx, args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "needletail", Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "needletail: unknown command %q\n%s", args[0], usage)
	return 2
}

// runtime is everything a turn needs, built once per process.
type runtime struct {
	cfg    *config.Config
	sec    *secrets.Secrets
	red    *secrets.Redactor
	store  *sessions.Store
	agent  *loop.Agent
	sched  *sched.Scheduler
	luau   *luau.Runtime // nil when the Luau engine could not start
	logger *log.Logger
	// guide is the system prompt's guide section (guide.go), rendered at boot and again once
	// the Slack gateway runs
	guide     guide
	guideText atomic.Pointer[string]
	// jail is how the agent's commands are confined, as the activity reports it (jailFor)
	jail string
	// dmUsers holds the Slack gateway's KnownDMUser once it runs (the memory's audiences)
	dmUsers *atomic.Pointer[func(channel string) string]
	// operators are the owner's own conversations (the memory's audiences): a oneshot's, marked
	// by this process when it creates it
	operators *operators
}

// operators are the conversations (their roots) this process started for the owner: a oneshot's.
// Kept in memory only, never read from a session's source, which the API or a file could claim.
type operators struct{ m sync.Map }

func (o *operators) mark(root string) { o.m.Store(root, true) }

func (o *operators) has(root string) bool {
	if o == nil {
		return false
	}
	_, ok := o.m.Load(root)
	return ok
}

// setGuide renders the guide section the turns read.
func (rt *runtime) setGuide() {
	text := rt.guide.render()
	rt.guideText.Store(&text)
}

// boot loads the config, takes the secrets out of the env (§4.2) and builds the agent.
func boot(home string, stderr io.Writer) (*runtime, error) {
	cfg, err := config.Load(home, os.Getenv)
	if err != nil {
		return nil, err
	}
	environ := os.Environ()
	sec := secrets.Load(&cfg.Config, environ)
	red := secrets.NewRedactor(sec, environ)
	sec.Scrub()
	// uid 10000 has no passwd entry: everything the runtime starts lives in the agent's home
	_ = os.Setenv("HOME", cfg.Home)
	logger := log.New(red.Writer(stderr), "", log.LstdFlags|log.LUTC)
	logf := func(s string) { logger.Println(s) }
	self := executable()
	// Started as root (the image), the runtime lays the home out and drops to its own user before
	// anything else touches the home; the agent's commands then run as another (§7).
	split, err := privsep.Enter(cfg.Home, cfg.StateRoot, logf)
	if err != nil {
		return nil, err
	}
	// Scrub changes only this process' own copy of the env: the kernel still shows the env it
	// started with in /proc/<pid>/environ, which a terminal command (same uid) could read.
	if err := undumpable(); err != nil {
		logf(fmt.Sprintf("could not hide the process from same-user readers (prctl): %v", err))
	}

	eps := []llm.Endpoint{}
	for _, e := range cfg.Endpoints() {
		eps = append(eps, llm.Endpoint{BaseURL: e.BaseURL, Key: sec.Get(e.KeyEnv), Model: e.Model})
	}
	client := &llm.Client{Endpoints: eps, Reasoning: cfg.Reasoning, Log: logf}
	store, err := sessions.Open(cfg.SessionsDir(), red.Redact)
	if err != nil {
		return nil, err
	}
	// The memory (§4.6): RECORDS.md read and reconciled with the hot files rehydrate wrote, before
	// the first turn. Secret values are refused in it, as in skills.
	mem := &memory.Store{Home: cfg.Home, Limits: map[string]int{memory.TargetMemory: cfg.Memory.MemoryCharLimit, memory.TargetUser: cfg.Memory.UserCharLimit},
		Check: tools.SecretCheck(red.Redact), Log: logf}
	dmUser := &atomic.Pointer[func(channel string) string]{}
	ops := &operators{}
	mem.Where = audiences(store, func(channel string) string {
		if f := dmUser.Load(); f != nil {
			return (*f)(channel)
		}
		return ""
	}, ops)
	if err := mem.Open(); err != nil {
		logf(fmt.Sprintf("memory: %v", err))
	}
	env := tools.Env{
		Home:           cfg.Home,
		Sandbox:        sandbox.Sandbox{Home: cfg.Home, Shared: cfg.SharedRoots()},
		CommandTimeout: cfg.CommandTimeout(),
		Environ:        func() []string { return sec.Clean(os.Environ()) },
		Memory:         mem,
		Skills:         &skills.Library{Home: cfg.Home, Generated: cfg.Skills.Generated, Check: tools.SecretCheck(red.Redact)},
		Redact:         red.Redact,
		LongestSecret:  red.Longest,
		// SharedWrite (the core's fs_note after a /shared write) is set by connectCore.
	}
	if split.Exec != nil {
		env.Agent = &tools.User{UID: uint32(split.Exec.ExecUID), GID: uint32(split.Exec.RuntimeGID)}
		// /tmp is outside the jail and, with a read-only root filesystem, not writable
		env.TmpDir = filepath.Join(cfg.Home, jail.TmpDir)
	}
	// The jail (§7): the agent's commands confined to the home and the shared layers.
	js := jailFor(split.Exec != nil, jailSpec(cfg), jail.ABI, self, os.Getenv, logf)
	env.Jail = js.launcher
	agent := &loop.Agent{Cfg: cfg, LLM: client, Store: store, Tools: tools.NewRegistry(tools.Builtins()...), Env: env, Log: logf}
	recall := recaller(cfg.Memory.Recall, mem, store, logf)
	agent.Recall = recall.Block
	lr := startLuau(cfg, sec, red, mem, env.Skills, agent, logf)
	// The scheduler (§4.7): its tool works in every command; serve gives it turns and ticks it.
	// Cron expressions run on the agent's wall clock: needletail.yaml's timezone, else TZ, else UTC.
	opts := sched.Options{Home: cfg.Home, Skills: env.Skills, Tools: agent.Tools.Names, Env: env,
		End: store.End, Location: cfg.Location, Log: logf, RepoJobs: cfg.Schedules.Repo}
	if lr != nil {
		opts.Luau = lr.RunScript
		opts.OnSchedule = luau.Events{RT: lr}.OnSchedule // the agent's on_schedule handlers (§5.5)
	}
	s := sched.New(opts)
	agent.Tools.Add(s.Tool(origin(store)))
	rt := &runtime{cfg: cfg, sec: sec, red: red, store: store, agent: agent, sched: s, luau: lr, logger: logger, dmUsers: dmUser, operators: ops,
		jail: js.state,
		guide: guide{Home: cfg.Home, Shared: cfg.SharedRoots(), Split: split.Exec != nil, Jailed: js.launcher != nil, TmpDir: env.TmpDir, Luau: lr != nil, Grants: cfg.Luau.Grants, LuauTimeout: cfg.Limits.LuauTimeout,
			ScriptTimeout: cfg.Limits.ScriptTimeout, Recall: recall.Auto}}
	rt.setGuide()
	// the guide comes first among the sections, before a channel's guidance
	agent.Sections = append([]func() string{func() string { return *rt.guideText.Load() }}, agent.Sections...)
	return rt, nil
}

// recaller is the per-turn recall (§4.6) as needletail.yaml's memory.recall sets it: on for user
// turns and off for scheduled runs unless it says otherwise.
func recaller(c *wire.RecallConfig, mem *memory.Store, store *sessions.Store, logf func(string)) *memory.Recaller {
	rc := &memory.Recaller{Store: mem, Root: store.Root, Auto: true, Log: logf}
	if c != nil {
		rc.Auto = c.Auto == nil || *c.Auto
		rc.Jobs, rc.Max, rc.Budget = c.Jobs, c.Max, c.Budget
	}
	return rc
}

// audiences is the memory's audience of a session's turns (memory.Audience, docs/needletail.md
// §4.6), from the runtime's own records, never the model's words: the session's root and source.
// A Slack thread's is its channel's scope, a Slack DM's its person's (the gateway's, dmUser: ""
// while it does not know them, and then nothing is saved "here"): only the gateway makes them,
// the API refusing the slack- ids and any conversation of source slack (api.Reserved). A
// oneshot's (the owner at the command line) is the operator's, known from ops, which this process
// marks when it creates the oneshot, never from a session's source ("cli" in a file is no one's).
// Every other turn (the API's runs, which the owner's chat and the core's wakes share; a
// scheduled job; a schedule's script) has none: the agent-wide records only. A session the store
// does not know yet (a message's handlers run before its turn creates it) is read from its id
// when it is a Slack one.
func audiences(store *sessions.Store, dmUser func(channel string) string, ops *operators) func(session string) memory.Audience {
	return func(session string) memory.Audience {
		root, source := session, ""
		if r, ok := store.Root(session); ok {
			h, _ := store.Get(r)
			root, source = r, h.Source
		} else if strings.HasPrefix(session, "slack-") {
			source = slack.Source
		}
		if ops.has(root) {
			return memory.Audience{Operator: true}
		}
		if source != slack.Source {
			return memory.Audience{}
		}
		channel, thread, ok := slack.Origin(root)
		if !ok {
			return memory.Audience{}
		}
		if thread != "" {
			return memory.Audience{Scope: memory.ChannelScope(channel), Conv: root}
		}
		a := memory.Audience{Conv: root}
		if p := dmUser(channel); p != "" {
			a.Scope = memory.DMScope(p)
		}
		return a
	}
}

// luauGroup is the registry group of the Luau tools (found again every turn).
const luauGroup = "luau"

// startLuau gives the agent its Luau tools (§5): found in its skills every turn, checked when
// skill_manage writes one, dry-run by luau_test, and run by schedules. Without the engine the
// agent runs without them.
func startLuau(cfg *config.Config, sec *secrets.Secrets, red *secrets.Redactor, mem *memory.Store, lib *skills.Library,
	agent *loop.Agent, logf func(string)) *luau.Runtime {
	eng, err := luau.Shared()
	if err != nil {
		logf(fmt.Sprintf("luau: the engine did not start, no Luau tools: %v", err))
		return nil
	}
	layers := []string{"group"}
	if cfg.Unit != "" {
		layers = append(layers, cfg.Unit)
	}
	lr := &luau.Runtime{Runner: eng, Home: cfg.Home, SharedDir: cfg.SharedDir, Layers: layers, Grants: cfg.Luau.Grants,
		HTTPSecrets: cfg.Luau.HTTPSecrets, Secret: sec.Get, Memory: mem, Redact: red.Redact, Log: logf,
		LuauTimeout: time.Duration(cfg.Limits.LuauTimeout) * time.Second, ScriptTimeout: time.Duration(cfg.Limits.ScriptTimeout) * time.Second}
	if u := os.Getenv("SWARM_CORE_URL"); u != "" {
		lr.CoreURLs = append(lr.CoreURLs, u)
	}
	if cfg.Core != nil {
		lr.CoreURLs = append(lr.CoreURLs, cfg.Core.MCPURL, cfg.Core.IngestURL)
	}
	// handler runs are hooked like tool calls, to whatever observes the turns (the core's hooks
	// connect after boot)
	lr.Observer = func() luau.HookObserver { return agent.Observer }
	reserved := func(name string) bool { return agent.Tools.Taken(luauGroup, name) }
	lib.CheckLuau = func(rel string, body []byte) error { return lr.CheckFile(context.Background(), rel, body) }
	lib.LuauTools = func(dir string) []skills.ToolStatus {
		return append(lr.Status(context.Background(), dir, reserved), lr.HandlerStatus(context.Background(), dir)...)
	}
	agent.Refresh = func(ctx context.Context) { agent.Tools.SetGroup(luauGroup, lr.Tools(ctx, reserved)) }
	agent.Tools.Add(lr.TestTool(reserved), luau.ReferenceTool())
	return lr
}

// luauCommand is `needletail luau check <file>` and `needletail luau reference`.
func luauCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "reference" {
		fmt.Fprint(stdout, luau.Reference())
		return 0
	}
	if len(args) != 2 || args[0] != "check" {
		fmt.Fprint(stderr, usage)
		return 2
	}
	file := args[1]
	body, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "needletail luau check: %v\n", err)
		return 1
	}
	eng, err := luau.Shared()
	if err != nil {
		fmt.Fprintf(stderr, "needletail luau check: %v\n", err)
		return 1
	}
	lr := &luau.Runtime{Runner: eng}
	// A tool (…/tools/<name>.luau) needs a tool's header, a handler (…/handlers/<name>.luau) a
	// handler's; any other file a script's.
	slash := filepath.ToSlash(file)
	tool := strings.Contains("/"+slash, "/tools/") && strings.HasSuffix(slash, ".luau")
	handler := strings.Contains("/"+slash, "/"+luau.HandlerDir+"/") && strings.HasSuffix(slash, ".luau")
	var h *luau.Header
	switch {
	case handler:
		err = lr.CheckHandler(ctx, luau.HandlerDir+"/"+filepath.Base(file), body)
		if err == nil {
			h, err = luau.ParseHeaderOf(body, luau.KindHandler)
		}
	case tool:
		err = lr.CheckTool(ctx, "tools/"+filepath.Base(file), body)
		if err == nil {
			h, err = luau.ParseHeader(body, true)
		}
	default:
		if h, err = luau.ParseHeader(body, false); err == nil {
			err = eng.Check(ctx, body)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", file, err)
		return 1
	}
	what := "script"
	switch {
	case handler:
		what = "handler " + h.Name + " for " + strings.Join(h.Events, ", ")
	case tool:
		what = "tool " + h.Name
	}
	fmt.Fprintf(stdout, "%s: ok (%s; caps: %s)\n", file, what, strings.Join(h.Caps, ", "))
	return 0
}

// origin is the chat a session belongs to, as a job's "origin" (Hermes' {platform, chat_id,
// thread_id}): a Slack thread's session (slack-<channel>-<thread_ts>), or a DM's
// (slack-<channel>-dm, no thread: the DM itself); none for the API's.
func origin(store *sessions.Store) func(session string) json.RawMessage {
	return func(session string) json.RawMessage {
		root, ok := store.Root(session)
		if !ok {
			return nil
		}
		h, _ := store.Get(root)
		channel, thread, ok := slack.Origin(root)
		if h.Source != slack.Source || !ok {
			return nil
		}
		o := map[string]any{"platform": "slack", "chat_id": channel, "thread_id": nil}
		if thread != "" {
			o["thread_id"] = thread
		}
		b, _ := json.Marshal(o)
		return b
	}
}

// reservations are the API server's turn slots as the scheduler takes them: its jobs' lane.
type reservations struct{ srv *api.Server }

func (r reservations) Reserve(session string, create wire.Session) (sched.Slot, error) {
	res, err := r.srv.ReserveJob(session, create) // the jobs' own lane, beside the chat's
	if err != nil {
		return nil, err
	}
	return res, nil
}

// slackTurns are the same slots as the Slack gateway takes them.
type slackTurns struct{ srv *api.Server }

func (r slackTurns) Reserve(session string, create wire.Session) (slack.Slot, error) {
	res, err := r.srv.Reserve(session, create)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// SlackAPIEnv overrides Slack's Web API URL; tests only: it must be a loopback address.
const SlackAPIEnv = "NEEDLETAIL_SLACK_API_URL"

// startSlack is the Slack gateway (§4.9), when needletail.yaml names the channel and both tokens
// are there; nil otherwise. Its settings are the channel's, in Hermes' SLACK_* env.
func startSlack(rt *runtime, srv *api.Server) *slack.Gateway {
	if !slices.Contains(rt.cfg.Channels, "slack") {
		return nil
	}
	bot, app := rt.sec.Get(secrets.SlackBot), rt.sec.Get(secrets.SlackApp)
	if bot == "" || app == "" {
		rt.logger.Printf("slack: the agent has a Slack channel but %s or %s is missing: the gateway is off", secrets.SlackBot, secrets.SlackApp)
		return nil
	}
	users := []string{}
	for _, u := range strings.Split(os.Getenv("SLACK_ALLOWED_USERS"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			users = append(users, u)
		}
	}
	store := rt.store
	var handlers messaging.Handlers // the agent's on_message, on_mention and before_send (§5.5)
	if rt.luau != nil {
		handlers = luau.Events{RT: rt.luau}
	}
	gw, err := slack.New(slack.Options{
		Web:          &slack.Web{BaseURL: os.Getenv(SlackAPIEnv), BotToken: bot, AppToken: app},
		Turns:        slackTurns{srv},
		Exists:       func(session string) bool { _, ok := store.Root(session); return ok },
		Root:         store.Root,
		Channels:     func() []string { return slackChannels(store) },
		AllowedUsers: users,
		AllowAll:     strings.EqualFold(os.Getenv("SLACK_ALLOW_ALL_USERS"), "true"),
		AllowBots:    os.Getenv("SLACK_ALLOW_BOTS"),
		HomeChannel:  os.Getenv("SLACK_HOME_CHANNEL"),
		Broadcast:    rt.cfg.Slack != nil && rt.cfg.Slack.Broadcast,
		Redact:       rt.red.Redact,
		Log:          func(s string) { rt.logger.Println(s) },
		Handlers:     handlers,
	})
	if err != nil {
		rt.logger.Printf("slack: %v: the gateway is off", err)
		return nil
	}
	rt.agent.Tools.Add(gw.Tools()...)
	rt.agent.Sections = append(rt.agent.Sections, gw.Guidance)
	rt.guide.Slack, rt.guide.Broadcast = true, rt.cfg.Slack != nil && rt.cfg.Slack.Broadcast
	rt.setGuide()
	rt.sched.SetDeliverer(gw)
	rt.sched.SetTargetCheck(gw.CheckTarget)
	if rt.luau != nil {
		rt.luau.Messenger = gw // Luau's send_message, under the same policy
	}
	known := gw.KnownDMUser
	rt.dmUsers.Store(&known) // the memory's DM audiences
	gw.Start()
	return gw
}

// slackChannels are the channels the agent has Slack conversations in (send_message's targets).
func slackChannels(store *sessions.Store) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range store.List(1000) {
		id := s.ID
		if s.Root != "" {
			id = s.Root
		}
		if ch, _, ok := slack.Origin(id); ok && !seen[ch] {
			seen[ch] = true
			out = append(out, ch)
		}
	}
	return out
}

func serve(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	home := fs.String("home", "", "agent home (default $AGENT_HOME, else /opt/data)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rt, err := boot(*home, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "needletail serve: %v\n", err)
		return 1
	}
	// Nothing writes through the temp dir yet: what a crash left there goes (oneshot, which may
	// run beside a serving runtime, never sweeps).
	if err := fsx.Sweep(rt.cfg.Home); err != nil {
		rt.logger.Printf("could not empty %s: %v", fsx.TmpDir, err)
	}
	key := rt.sec.Get(secrets.APIKey)
	if len(key) < api.MinKey {
		rt.logger.Printf("%s is missing or shorter than %d characters: the API answers only /health", secrets.APIKey, api.MinKey)
	}
	link := connectCore(rt)
	defer link.close(3 * time.Second)
	var gw *slack.Gateway
	srv, err := api.New(api.Options{Key: key, Store: rt.store, Agent: rt.agent, StateDir: rt.cfg.StateDir(), TmpDir: rt.cfg.TmpDir(),
		Log: func(s string) { rt.logger.Println(s) }, Redact: rt.red.Redact, Jobs: rt.sched.Running, JobTurns: rt.cfg.Schedules.Turns, Jail: rt.jail,
		Platforms: func() map[string]wire.Platform {
			if gw == nil {
				return nil
			}
			return map[string]wire.Platform{"slack": gw.Platform()}
		}})
	if err != nil {
		fmt.Fprintf(stderr, "needletail serve: %v\n", err)
		return 1
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(rt.cfg.Port))
	if err != nil {
		fmt.Fprintf(stderr, "needletail serve: %v\n", err)
		return 1
	}
	rt.sched.SetTurns(reservations{srv})
	gw = startSlack(rt, srv)
	ticks, stopTicks := context.WithCancel(context.Background())
	defer stopTicks()
	rt.sched.Start(ticks)
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, ErrorLog: rt.logger}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	rt.logger.Printf("needletail %s serving agent %s on :%d (model endpoints: %d)", Version, rt.cfg.Agent, rt.cfg.Port, len(rt.cfg.Endpoints()))
	select {
	case <-ctx.Done():
	case err := <-errc:
		rt.logger.Printf("server stopped: %v", err)
		return 1
	}
	rt.logger.Printf("shutting down")
	stopTicks() // no job starts from here on
	if gw != nil {
		gw.BeginShutdown() // no Slack message starts a turn; new events are left for Slack to resend
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = hs.Shutdown(sctx)
	cancel()
	dctx, cancel := context.WithTimeout(context.Background(), drainTurns)
	srv.Shutdown(dctx) // jobs' and Slack's turns drain (or are cancelled) with the API's
	cancel()
	jctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	rt.sched.Shutdown(jctx) // and script-only jobs get a few seconds more (deliveries included)
	cancel()
	if gw != nil {
		gctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		gw.Shutdown(gctx) // the drained turns' replies are posted, then the socket closes
		cancel()
	}
	rt.agent.Env.Memory.Flush() // the memory's use counts
	return 0
}

// streamJSON writes oneshot's events as Hermes' stream-json lines (pkg/engine/needletail ParseBench).
type streamJSON struct{ enc *json.Encoder }

func (s streamJSON) ToolStart(_ string, c wire.ToolCall) {
	_ = s.enc.Encode(map[string]any{"type": "tool_use", "name": c.Name, "id": c.ID})
}
func (s streamJSON) ToolDone(string, wire.ToolCall, string, bool, time.Duration) {}
func (s streamJSON) Text(_ string, text string) {
	_ = s.enc.Encode(map[string]any{"type": "text", "text": text})
}

func oneshot(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("oneshot", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prompt := fs.String("q", "", "the prompt")
	format := fs.String("format", "text", "output: text or stream-json")
	home := fs.String("home", "", "agent home (default $AGENT_HOME, else /opt/data)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*prompt) == "" || (*format != "text" && *format != "stream-json") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	rt, err := boot(*home, stderr)
	if err != nil {
		return oneshotResult(stdout, stderr, *format, nil, err)
	}
	h, err := rt.store.Create(wire.Session{ID: "oneshot-" + sessions.NewID(), Source: "cli"})
	if err != nil {
		return oneshotResult(stdout, stderr, *format, nil, err)
	}
	rt.operators.mark(h.ID) // the owner's own turn: every scope (§4.6)
	opts := loop.Options{}
	if *format == "stream-json" {
		opts.Observer = streamJSON{json.NewEncoder(stdout)}
	}
	res, err := rt.agent.Run(ctx, h.ID, *prompt, opts)
	if err == nil {
		_ = rt.store.End(res.Session, "oneshot")
	}
	rt.agent.Env.Memory.Flush()
	return oneshotResult(stdout, stderr, *format, res, err)
}

func oneshotResult(stdout, stderr io.Writer, format string, res *loop.Result, err error) int {
	code := 0
	if err != nil {
		code = 1
	}
	if format == "stream-json" {
		out := map[string]any{"type": "result", "exit_code": code, "error": ""}
		if res != nil {
			out["text"] = res.Text
			out["tokens"] = map[string]int{"total": res.Tokens}
			out["session_id"] = res.Session
		}
		if err != nil {
			out["error"] = err.Error()
		}
		_ = json.NewEncoder(stdout).Encode(out)
		return code
	}
	if err != nil {
		fmt.Fprintf(stderr, "needletail oneshot: %v\n", err)
		return code
	}
	fmt.Fprintln(stdout, res.Text)
	return code
}

func healthcheck(stderr io.Writer) int {
	port := strconv.Itoa(config.DefaultPort)
	if p := os.Getenv("NEEDLETAIL_API_PORT"); p != "" {
		port = p
	}
	c := &http.Client{Timeout: 3 * time.Second}
	res, err := c.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		fmt.Fprintf(stderr, "unhealthy: %v\n", err)
		return 1
	}
	defer res.Body.Close()
	var body struct {
		Status string `json:"status"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body) != nil || body.Status != "ok" {
		fmt.Fprintf(stderr, "unhealthy: HTTP %d\n", res.StatusCode)
		return 1
	}
	return 0
}
