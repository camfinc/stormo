package core

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
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
	"github.com/camfinc/stormo/pkg/secrets"
	"github.com/camfinc/stormo/pkg/version"
)

// The core service (docs/core.md). A host process on loopback; agent containers reach it as
// host.docker.internal:18600 (on Linux through a bridge listener, bridge.go). It serves the LLM gateway and the office UI over the fleet registry;
// activity, workdir and comms routes land here in later phases.

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

var avatarPath = regexp.MustCompile(`^/avatars/([a-z][a-z0-9-]*)\.png$`)

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
	return c.Server.Close()
}

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
	if g.Keys == nil {
		g.Keys = NewAgentKeys(secrets.Path(inst.Root))
	}
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
			agents = append(agents, OfficeAgent{ID: a.ID, State: a.State, Activity: a.Activity})
		}
		office.Tick(agents, gateway.Status().Active, time.Now().UnixMilli())
	}
	c := &Core{Gateway: gateway, Fleet: fleet, Office: office, stop: make(chan struct{})}
	if fleet != nil {
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-c.stop:
					if startedFleet {
						fleet.Stop()
					}
					return
				case <-t.C:
					officeTick()
				}
			}
		}()
	}

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
			for _, a := range fs.Agents {
				view.Agents = append(view.Agents, fleetAgentView{a, office.State(a.ID)})
			}
			writeJSONBody(w, 200, view)
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
