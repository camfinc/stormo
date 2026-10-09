package core

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/core/llm"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/secrets"
	"github.com/camfinc/stormo/pkg/shared"
	"github.com/camfinc/stormo/pkg/version"
)

// The core service (docs/core.md). A host process on loopback; agent containers reach it as
// host.docker.internal:18600 (on Linux through a bridge listener, bridge.go). It serves the LLM
// gateway, the agents' hook ingest and the office UI over the fleet registry, and keeps its
// records in core.db.

// Port is the core's default port.
const Port = 18600

// CorePort is SWARM_CORE_PORT, else Port.
func CorePort() int {
	if n, err := strconv.Atoi(os.Getenv("SWARM_CORE_PORT")); err == nil && n > 0 {
		return n
	}
	return Port
}

// CoreDir is the core's state under the instance (.swarm/core).
func CoreDir(root string) string { return filepath.Join(root, ".swarm", "core") }

// AuthDir holds the one ChatGPT sign-in (0600, gitignored under .swarm/), local and under the
// owner's control.
func AuthDir(root string) string { return filepath.Join(CoreDir(root), "auth") }

var (
	//go:embed ui/index.html
	indexHTML string
	//go:embed ui/app.js
	appJS []byte
	//go:embed ui/style.css
	styleCSS []byte
)

var uiFiles = map[string]struct {
	body []byte
	ct   string
}{
	"/ui/app.js":    {appJS, "text/javascript;charset=utf-8"},
	"/ui/style.css": {styleCSS, "text/css;charset=utf-8"},
}

var (
	avatarPath = regexp.MustCompile(`^/avatars/([a-z][a-z0-9-]*)\.png$`)
	agentPath  = regexp.MustCompile(`^/api/agents/([a-z][a-z0-9-]*)/(timeline|activity)$`)
)

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")

// CoreOptions configure StartCore.
type CoreOptions struct {
	Inst *instance.Instance
	// Port to listen on (127.0.0.1); 0 picks a free one.
	Port int
	// Bridge are extra hosts to listen on, same port, for agent containers (BridgeHosts); they
	// serve only the model gateway and /health.
	Bridge []string
	// Gateway options; Auth and Keys default to the instance's sign-in and core keys.
	Gateway llm.GatewayOptions
	// Fleet registry for the UI; nil starts one unless NoFleet (tests skip the compose poller).
	Fleet   *Fleet
	NoFleet bool
	// DB is core.db; nil opens the instance's (.swarm/core/core.db).
	DB *DB
	// Keys map core keys to agents; nil reads the instance's secrets file.
	Keys *AgentKeys
	// OwnerToken authorises owner reads; "" reads (or mints) .swarm/core/owner.token.
	OwnerToken string
	// The bus's view of the fleet, wake runs and Slack mirror; nil takes the fleet's, Hermes'
	// /v1/runs with each agent's API key, and its Slack home channel.
	Roster   func() []BusAgent
	Wake     WakeFn
	Mirror   MirrorFn
	NoMirror bool
	// WorkdirRoot is the shared space the core tracks; "" is the instance's workdir/.
	WorkdirRoot string
	// LearnDeps are the learning cycle's nap, dream and counts; zero values take the real ones.
	LearnDeps LearnDeps
}

// Core is a running core service.
type Core struct {
	Server  *http.Server
	Addr    *net.TCPAddr
	Bridges []*net.TCPAddr
	bridge  []*http.Server
	Gateway *llm.Gateway
	Fleet   *Fleet
	Office  *Office
	DB      *DB
	Monitor *Monitor
	Bus     *Bus
	Workdir *Workdir
	Learner *Learner
	MCP     *MCP
	keys    *AgentKeys
	owner   string
	ownDB   bool
	stop    chan struct{}
	once    sync.Once
}

// StopOffice stops the office timer (and the fleet's timers if the core started them).
func (c *Core) StopOffice() { c.once.Do(func() { close(c.stop) }) }

// Close stops the office and the HTTP servers.
func (c *Core) Close() error {
	c.StopOffice()
	for _, b := range c.bridge {
		_ = b.Close()
	}
	err := c.Server.Close()
	if c.ownDB {
		c.DB.Close()
	}
	return err
}

// caller is who a request comes from: the owner (owner token) or an agent (its core key).
func (c *Core) caller(r *http.Request) Caller { return callerOf(r, c.owner, c.keys) }

func marshalCompact(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func writeJSONBody(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(marshalCompact(v))
}

func errBody(code, msg string) map[string]any {
	return map[string]any{"error": map[string]string{"code": code, "message": msg}}
}

// fleetAgentView is a fleet agent with the office's state for it, as /api/fleet serves it.
type fleetAgentView struct {
	FleetAgent
	Office *OfficeState `json:"office"`
	// From the agent's hooks: the tool it is running now. nil until it posts one.
	Live *Live `json:"live"`
	// Its swarm messages: unread, and received and sent in the last day. Counts only.
	Messages BusCounts `json:"messages"`
	// Shared-space locks it holds (count only: paths can name clients).
	Locks int `json:"locks"`
}

type gatewayView struct {
	Login            llm.LoginState            `json:"login"`
	PlanLimitedUntil *string                   `json:"planLimitedUntil"`
	ManageUsageURL   string                    `json:"manageUsageUrl"`
	Inflight         int                       `json:"inflight"`
	Queued           int                       `json:"queued"`
	Concurrency      int                       `json:"concurrency"`
	Usage            map[string]llm.AgentUsage `json:"usage"`
	Active           map[string]int            `json:"active"`
}

type fleetView struct {
	PolledAt *string          `json:"polledAt"`
	Units    []FleetUnit      `json:"units"`
	Agents   []fleetAgentView `json:"agents"`
	Now      int64            `json:"now"`
	Robot    RobotState       `json:"robot"`
	Gateway  gatewayView      `json:"gateway"`
}

// instanceView is the instance's look: the page's window.STORMO and /api/instance.
type instanceView struct {
	Name   string                         `json:"name"`
	Org    string                         `json:"org"`
	Slug   string                         `json:"slug"`
	Clocks []instance.Clock               `json:"clocks"`
	Units  map[string]instance.OfficeUnit `json:"units"`
}

type coreInstance struct {
	Root string `json:"root"`
	Name string `json:"name"`
	Org  string `json:"org"`
	Slug string `json:"slug"`
}

// coreView is /api/core: the running core's identity.
type coreView struct {
	Service   string       `json:"service"`
	Version   string       `json:"version"`
	API       int          `json:"api"`
	PID       int          `json:"pid"`
	Exe       string       `json:"exe"`
	Port      int          `json:"port"`
	StartedAt string       `json:"startedAt"`
	Instance  coreInstance `json:"instance"`
}

// StartCore listens on 127.0.0.1 (and o.Bridge) and serves until Close.
func StartCore(o CoreOptions) (*Core, error) {
	inst := o.Inst
	g := o.Gateway
	if g.Auth == nil {
		g.Auth = llm.NewChatGPTAuth(llm.AuthOptions{Paths: llm.Paths(AuthDir(inst.Root))})
	}
	keys := o.Keys
	if keys == nil {
		keys = NewAgentKeys(secrets.Path(inst.Root))
	}
	if g.Keys == nil {
		g.Keys = keys
	}
	db, ownDB := o.DB, false
	if db == nil {
		var err error
		if db, err = OpenDB(DBPath(inst.Root)); err != nil {
			return nil, err
		}
		ownDB = true
	}
	owner := o.OwnerToken
	if owner == "" {
		var err error
		if owner, err = EnsureOwnerToken(inst.Root); err != nil {
			if ownDB {
				db.Close()
			}
			return nil, err
		}
	}
	monitor := NewMonitor(db, keys, Scrubber(learning.NewScrubber(inst)))
	if g.Concurrency == 0 {
		if n, err := strconv.Atoi(os.Getenv("SWARM_CORE_LLM_CONCURRENCY")); err == nil && n > 0 {
			g.Concurrency = n
		}
	}
	gateway := llm.NewGateway(g)
	fleet := o.Fleet
	startedFleet := false
	if fleet == nil && !o.NoFleet {
		fleet = NewFleet(FleetOptions{Inst: inst}).Start()
		startedFleet = true
	}
	// The office simulation runs here, once, so every viewer plays back the same thing.
	office := NewOffice(nil)
	officeTick := func() {
		if fleet == nil {
			return
		}
		for _, e := range fleet.TakeNapEvents() {
			office.NoteNap(e)
		}
		agents := []OfficeAgent{}
		for _, a := range fleet.Snapshot().Agents {
			if a.State != "running" && a.State != "starting" {
				monitor.Forget(a.ID) // its open tool calls will never finish
			}
			live := monitor.Live(a.ID)
			agents = append(agents, OfficeAgent{ID: a.ID, State: a.State, Activity: a.Activity, Busy: live != nil && live.Running > 0})
		}
		office.Tick(agents, gateway.Status().Active, time.Now().UnixMilli())
	}
	busy := func(a FleetAgent) bool {
		l := monitor.Live(a.ID)
		return l != nil && l.Running > 0 ||
			a.Activity != nil && (a.Activity.ActiveAgents > 0 || a.Activity.GatewayBusy || len(a.Activity.RunningJobs) > 0) ||
			gateway.Status().Active[a.ID] > 0
	}
	bo := BusOptions{DB: db, Roster: o.Roster, Wake: o.Wake, Mirror: o.Mirror}
	if bo.Roster == nil {
		bo.Roster = func() []BusAgent {
			out := []BusAgent{}
			if fleet == nil {
				return out
			}
			for _, a := range fleet.Snapshot().Agents {
				out = append(out, BusAgent{ID: a.ID, Unit: a.Unit, Running: a.State == "running", Busy: busy(a), Endpoint: a.Endpoint})
			}
			return out
		}
	}
	if bo.Wake == nil && fleet != nil {
		bo.Wake = HermesWake(fleet.APIKey, nil)
	}
	if bo.Mirror == nil && !o.NoMirror {
		bo.Mirror = SlackMirror(inst, nil)
	}
	bus := NewBus(bo)
	wroot := o.WorkdirRoot
	if wroot == "" {
		wroot = shared.LocalDir(inst.Root)
	}
	workdir := NewWorkdir(WorkdirOptions{Root: wroot, DB: db, Monitor: monitor, Roster: bo.Roster})
	mcp := NewMCP(keys, BusTools(bus), WorkdirTools(workdir))
	learner := NewLearner(LearnerOptions{Inst: inst, DB: db, Roster: bo.Roster, Deps: o.LearnDeps})
	c := &Core{Gateway: gateway, Fleet: fleet, Office: office, DB: db, Monitor: monitor, Bus: bus, Workdir: workdir, Learner: learner, MCP: mcp,
		keys: keys, owner: owner, ownDB: ownDB, stop: make(chan struct{})}
	go bus.Run(c.stop, 5*time.Second)
	go workdir.Run(c.stop, 5*time.Second)
	go learner.Run(c.stop, time.Minute)
	go func() {
		office := time.NewTicker(time.Second)
		prune := time.NewTicker(time.Hour)
		defer office.Stop()
		defer prune.Stop()
		_ = monitor.Prune()
		for {
			select {
			case <-c.stop:
				if startedFleet {
					fleet.Stop()
				}
				return
			case <-office.C:
				officeTick()
			case <-prune.C:
				if err := monitor.Prune(); err != nil {
					log.Printf("core.db prune: %v", err)
				}
				if err := bus.Prune(RetainMessages); err != nil {
					log.Printf("core.db prune: %v", err)
				}
			}
		}
	}()

	index := strings.ReplaceAll(indexHTML, "{{name}}", htmlEscaper.Replace(inst.Name))
	clocks := inst.Clocks
	if clocks == nil {
		clocks = []instance.Clock{}
	}
	units := inst.OfficeUnits
	if units == nil {
		units = map[string]instance.OfficeUnit{}
	}
	// The instance's look, for the page (window.STORMO) and other clients (/api/instance).
	look := instanceView{inst.Name, inst.Org, inst.Slug, clocks, units}
	cfg := marshalCompact(look)
	index = strings.Replace(index, "{{instance}}", strings.ReplaceAll(string(cfg), "<", `<`), 1)
	startedAt := time.Now().UTC().Format(time.RFC3339)

	handler := func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("core error: %v", rec)
				writeJSONBody(w, 500, errBody("core_internal", "internal error"))
			}
		}()
		path := r.URL.Path
		get := r.Method == http.MethodGet
		switch {
		case get && path == "/health":
			s := gateway.Status()
			writeJSONBody(w, 200, map[string]any{"status": "ok", "service": "swarm-core", "login": s.Login, "planLimitedUntil": s.PlanLimitedUntil,
				"version": version.String(), "api": version.API})
			return
		case get && path == "/api/core":
			// Which binary and which instance this core is, for clients deciding whether to attach.
			// Loopback only: host paths never reach agent containers (bridge listeners serve no /api).
			writeJSONBody(w, 200, coreView{"swarm-core", version.String(), version.API, os.Getpid(), version.Exe(), c.Addr.Port, startedAt,
				coreInstance{inst.Root, inst.Name, inst.Org, inst.Slug}})
			return
		case get && path == "/api/instance":
			writeJSONBody(w, 200, look)
			return
		case get && path == "/api/gateway":
			// Loopback only (bridge listeners never route /api); no secrets in it.
			writeJSONBody(w, 200, gateway.Status())
			return
		case get && path == "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write([]byte(index))
			return
		case get && path == "/api/fleet":
			// No account email: the office shows plan state, not who signed in.
			s := gateway.Status()
			officeTick()
			fs := FleetSnapshot{Units: []FleetUnit{}, Agents: []FleetAgent{}}
			if fleet != nil {
				fs = fleet.Snapshot()
			}
			view := fleetView{PolledAt: fs.PolledAt, Units: fs.Units, Agents: []fleetAgentView{}, Now: time.Now().UnixMilli(), Robot: office.Robot(),
				Gateway: gatewayView{s.Login, s.PlanLimitedUntil, s.ManageUsageURL, s.Inflight, s.Queued, s.Concurrency, s.Usage, s.Active}}
			counts, _ := bus.Counts()
			locks := workdir.LockCounts()
			for _, a := range fs.Agents {
				view.Agents = append(view.Agents, fleetAgentView{a, office.State(a.ID), monitor.Live(a.ID), counts[a.ID], locks[a.ID]})
			}
			writeJSONBody(w, 200, view)
			return
		case path == "/ingest/hermes":
			monitor.Handle(w, r)
			return
		case path == "/mcp":
			mcp.Handle(w, r)
			return
		case get && path == "/api/learning":
			// Schedule and counts per agent: no lesson text, so no key.
			v, err := learner.View(10)
			if err != nil {
				writeJSONBody(w, 500, errBody("core_internal", "could not read learning cycles"))
				return
			}
			writeJSONBody(w, 200, v)
			return
		case r.Method == http.MethodPost && path == "/api/learn":
			// Writes agents' learnings in the working tree: the owner only.
			if !c.caller(r).Owner {
				writeJSONBody(w, 401, errBody("unauthorized", "the owner token is required"))
				return
			}
			var req struct {
				Agents []string `json:"agents"`
				Force  bool     `json:"force"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil && err != io.EOF {
				writeJSONBody(w, 400, errBody("bad_request", "body is {agents, force}"))
				return
			}
			id, err := learner.Start("manual", req.Agents, req.Force)
			if errors.Is(err, ErrCycleRunning) {
				writeJSONBody(w, 409, errBody("conflict", err.Error()))
				return
			}
			if err != nil {
				writeJSONBody(w, 400, errBody("bad_request", err.Error()))
				return
			}
			writeJSONBody(w, 202, map[string]any{"cycle": id})
			return
		case get && path == "/api/workdir":
			// File names can name clients: the owner only.
			if !c.caller(r).Owner {
				writeJSONBody(w, 401, errBody("unauthorized", "the owner token is required"))
				return
			}
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			v, err := workdir.Owner(limit)
			if err != nil {
				writeJSONBody(w, 500, errBody("core_internal", "could not read the workdir"))
				return
			}
			writeJSONBody(w, 200, v)
			return
		case get && (path == "/api/messages" || path == "/api/findings"):
			// Bodies, subjects and evidence can hold client data: the owner only.
			if !c.caller(r).Owner {
				writeJSONBody(w, 401, errBody("unauthorized", "the owner token is required"))
				return
			}
			q := r.URL.Query()
			limit, _ := strconv.Atoi(q.Get("limit"))
			if path == "/api/messages" {
				threads, err := bus.Threads(q.Get("agent"), limit, true)
				if err != nil {
					writeJSONBody(w, 500, errBody("core_internal", "could not read messages"))
					return
				}
				writeJSONBody(w, 200, map[string]any{"threads": threads})
				return
			}
			if limit <= 0 || limit > 500 {
				limit = 100
			}
			fs, err := db.Findings(q.Get("all") == "1", limit)
			if err != nil {
				writeJSONBody(w, 500, errBody("core_internal", "could not read findings"))
				return
			}
			writeJSONBody(w, 200, map[string]any{"findings": fs})
			return
		}
		if m := agentPath.FindStringSubmatch(path); m != nil && get {
			// timeline: event, tool and time only (the office's agent page). activity: with sessions
			// and previews, which can hold client data: the owner, or that agent itself.
			private := m[2] == "activity"
			if private {
				who := c.caller(r)
				if !who.Owner && who.Agent != m[1] {
					writeJSONBody(w, 401, errBody("unauthorized", "activity previews need the owner token or the agent's own key"))
					return
				}
			}
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			rows, err := monitor.Timeline(m[1], limit, private)
			if err != nil {
				writeJSONBody(w, 500, errBody("core_internal", "could not read the timeline"))
				return
			}
			writeJSONBody(w, 200, map[string]any{"agent": m[1], "live": monitor.Live(m[1]), "entries": rows})
			return
		}
		if f, ok := uiFiles[path]; ok && get {
			w.Header().Set("Content-Type", f.ct)
			w.Header().Set("Cache-Control", "no-cache")
			_, _ = w.Write(f.body)
			return
		}
		if m := avatarPath.FindStringSubmatch(path); m != nil && get {
			file := ""
			if fleet != nil {
				file = fleet.AvatarPath(m[1])
			}
			body, err := os.ReadFile(file)
			if file == "" || err != nil {
				writeJSONBody(w, 404, errBody("not_found", "no avatar"))
				return
			}
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "max-age=300")
			_, _ = w.Write(body)
			return
		}
		if gateway.Handle(w, r) {
			return
		}
		writeJSONBody(w, 404, errBody("not_found", r.Method+" "+path))
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", o.Port))
	if err != nil {
		c.StopOffice()
		return nil, err
	}
	c.Addr = ln.Addr().(*net.TCPAddr)
	bridged := func(w http.ResponseWriter, r *http.Request) {
		if !bridgeRoute(r.URL.Path) {
			writeJSONBody(w, 404, errBody("not_found", r.Method+" "+r.URL.Path))
			return
		}
		handler(w, r)
	}
	bridges := []net.Listener{}
	for _, h := range o.Bridge {
		bl, err := net.Listen("tcp", net.JoinHostPort(h, strconv.Itoa(c.Addr.Port)))
		if err != nil {
			for _, l := range append(bridges, ln) {
				l.Close()
			}
			c.StopOffice()
			return nil, fmt.Errorf("core bridge listener %s (SWARM_CORE_BIND): %w", h, err)
		}
		bridges = append(bridges, bl)
		c.Bridges = append(c.Bridges, bl.Addr().(*net.TCPAddr))
	}
	// Streams can sit silent while the model thinks; the gateway enforces its own timeouts.
	c.Server = &http.Server{Handler: http.HandlerFunc(handler), ReadHeaderTimeout: 30 * time.Second}
	serve := func(s *http.Server, l net.Listener) {
		if err := s.Serve(l); err != nil && err != http.ErrServerClosed {
			log.Printf("core server: %v", err)
		}
	}
	go serve(c.Server, ln)
	for _, bl := range bridges {
		b := &http.Server{Handler: http.HandlerFunc(bridged), ReadHeaderTimeout: 30 * time.Second}
		c.bridge = append(c.bridge, b)
		go serve(b, bl)
	}
	return c, nil
}
