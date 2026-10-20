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
	"strconv"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/api"
	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/llm"
	"github.com/camfinc/stormo/internal/needletail/loop"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/secrets"
	"github.com/camfinc/stormo/internal/needletail/sessions"
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
	case "serve":
		return serve(ctx, args[1:], stderr)
	case "oneshot":
		return oneshot(ctx, args[1:], stdout, stderr)
	case "healthcheck":
		return healthcheck(stderr)
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
	logger *log.Logger
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
	mem := &memory.Store{Home: cfg.Home, Limits: map[string]int{memory.TargetMemory: cfg.Memory.MemoryCharLimit, memory.TargetUser: cfg.Memory.UserCharLimit}}
	env := tools.Env{
		Home:           cfg.Home,
		Sandbox:        sandbox.Sandbox{Home: cfg.Home, Shared: cfg.SharedRoots()},
		CommandTimeout: cfg.CommandTimeout(),
		Environ:        func() []string { return sec.Clean(os.Environ()) },
		Memory:         mem,
		Redact:         red.Redact,
		LongestSecret:  red.Longest,
		// SharedWrite (the core's fs_note after a /shared write) is set by connectCore.
	}
	agent := &loop.Agent{Cfg: cfg, LLM: client, Store: store, Tools: tools.NewRegistry(tools.Builtins()...), Env: env, Log: logf}
	return &runtime{cfg: cfg, sec: sec, red: red, store: store, agent: agent, logger: logger}, nil
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
	key := rt.sec.Get(secrets.APIKey)
	if len(key) < api.MinKey {
		rt.logger.Printf("%s is missing or shorter than %d characters: the API answers only /health", secrets.APIKey, api.MinKey)
	}
	link := connectCore(rt)
	defer link.close(3 * time.Second)
	srv, err := api.New(api.Options{Key: key, Store: rt.store, Agent: rt.agent, StateDir: rt.cfg.StateDir(),
		Log: func(s string) { rt.logger.Println(s) }, Redact: rt.red.Redact})
	if err != nil {
		fmt.Fprintf(stderr, "needletail serve: %v\n", err)
		return 1
	}
	ln, err := net.Listen("tcp", ":"+strconv.Itoa(rt.cfg.Port))
	if err != nil {
		fmt.Fprintf(stderr, "needletail serve: %v\n", err)
		return 1
	}
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
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = hs.Shutdown(sctx)
	cancel()
	dctx, cancel := context.WithTimeout(context.Background(), drainTurns)
	srv.Shutdown(dctx)
	cancel()
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
	opts := loop.Options{}
	if *format == "stream-json" {
		opts.Observer = streamJSON{json.NewEncoder(stdout)}
	}
	res, err := rt.agent.Run(ctx, h.ID, *prompt, opts)
	if err == nil {
		_ = rt.store.End(res.Session, "oneshot")
	}
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
