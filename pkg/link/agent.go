package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/camfinc/stormo/pkg/engine"
)

// AgentOptions configure the agent's end: the relay its engine calls, and the link to the core.
type AgentOptions struct {
	Agent     string
	Placement string
	Engine    string
	Version   string
	// CoreURL is the core's base URL (http or https); CoreKey the agent's core key.
	CoreURL string
	CoreKey string
	// AgentAPI is the agent's own engine API (http://agent:8642 locally, http://localhost:8642 on
	// ECS) and APIKey its key: status polls and wakes go there.
	AgentAPI string
	APIKey   string
	Runtime  engine.Runtime
	// Spool holds hook deliveries and queued messages while the core is away.
	Spool *Spool
	// Nap takes a nap now (the sidecar's own NapOnce).
	Nap func(reason string) error
	// Listen is the relay's address (default :RelayPort).
	Listen string
	// StatusEvery is how often status goes up (default 10 s).
	StatusEvery time.Duration
	Client      *http.Client
	Logf        func(format string, args ...any)
}

// Agent is a running agent end.
type Agent struct {
	o         AgentOptions
	client    *http.Client
	mu        sync.Mutex
	connected bool
	rtState   any
}

// NewAgent prepares an agent end; Run starts it.
func NewAgent(o AgentOptions) *Agent {
	if o.Listen == "" {
		o.Listen = ":" + strconv.Itoa(RelayPort)
	}
	if o.StatusEvery == 0 {
		o.StatusEvery = 10 * time.Second
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	o.CoreURL = strings.TrimRight(o.CoreURL, "/")
	c := o.Client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}
	return &Agent{o: o, client: c}
}

// Connected reports whether the link is up now.
func (a *Agent) Connected() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.connected
}

// Run serves the relay and keeps the link up until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", a.o.Listen)
	if err != nil {
		return fmt.Errorf("link relay %s: %w", a.o.Listen, err)
	}
	srv := &http.Server{Handler: a.Relay(), ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() { <-ctx.Done(); _ = srv.Close() }()
	go a.drainLoop(ctx)
	a.linkLoop(ctx)
	return nil
}

// Relay is the handler the agent's engine calls instead of the core: /ingest/* and /mcp go on to
// the core; what the core cannot take now is held (hooks always, msg_send calls) or refused fast.
func (a *Agent) Relay() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/health":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"status":"ok","link":%t,"buffered":%d}`, a.Connected(), a.o.Spool.Len())
		case strings.HasPrefix(r.URL.Path, "/ingest/") && r.Method == http.MethodPost:
			a.relayIngest(w, r)
		case r.URL.Path == "/mcp":
			a.relayMCP(w, r)
		default:
			http.Error(w, "not relayed", http.StatusNotFound)
		}
	})
}

var keptHeaders = []string{"Content-Type", "Authorization", "Accept", "Mcp-Protocol-Version"}

// hookHeader is an engine's own hook header, whatever the engine is called:
// X-<Engine>-Signature[-…], X-<Engine>-Event or X-<Engine>-Delivery. The core verifies a hook with
// the signature header its engine's runtime names (engine.Runtime.HookSignature), so the relay
// passes every engine's on, not only one engine's.
var hookHeader = regexp.MustCompile(`^X-[A-Za-z0-9]+-(Signature(-[A-Za-z0-9]+)*|Event|Delivery)$`)

// maxHookHeader bounds one hook header's value (a signature is under a hundred bytes).
const maxHookHeader = 512

func headersOf(r *http.Request) map[string]string {
	h := map[string]string{}
	for _, k := range keptHeaders {
		if v := r.Header.Get(k); v != "" {
			h[k] = v
		}
	}
	for k, vs := range r.Header {
		if len(vs) > 0 && vs[0] != "" && len(vs[0]) <= maxHookHeader && hookHeader.MatchString(k) {
			h[k] = vs[0]
		}
	}
	return h
}

// forward sends an item to the core; a transport error or a 5xx is "the core is away".
func (a *Agent) forward(ctx context.Context, it Item) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.o.CoreURL+it.Path, bytes.NewReader(it.Body))
	if err != nil {
		return nil, err
	}
	for k, v := range it.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Bearer "+a.o.CoreKey)
	}
	res, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 500 {
		res.Body.Close()
		return nil, fmt.Errorf("core answered %d", res.StatusCode)
	}
	return res, nil
}

func (a *Agent) relayIngest(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	it := Item{Path: r.URL.Path, Headers: headersOf(r), Body: body}
	// Older deliveries go first: while anything waits, this one waits behind it.
	if a.o.Spool.Len() == 0 {
		if res, err := a.forward(r.Context(), it); err == nil {
			defer res.Body.Close()
			w.WriteHeader(res.StatusCode)
			_, _ = io.Copy(w, io.LimitReader(res.Body, 64<<10))
			return
		}
	}
	if err := a.o.Spool.Put(it); err != nil {
		a.o.Logf("link: spool: %v", err)
	}
	w.WriteHeader(http.StatusAccepted)
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		Name string `json:"name"`
	} `json:"params"`
}

func (a *Agent) relayMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed) // no server stream, as the core
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	it := Item{Path: "/mcp", Headers: headersOf(r), Body: body}
	res, err := a.forward(r.Context(), it)
	if err == nil {
		defer res.Body.Close()
		for _, k := range []string{"Content-Type", "Mcp-Protocol-Version"} {
			if v := res.Header.Get(k); v != "" {
				w.Header().Set(k, v)
			}
		}
		w.WriteHeader(res.StatusCode)
		_, _ = io.Copy(w, res.Body)
		return
	}
	// The core is away. A message to another agent is kept and delivered later; anything else
	// fails now, so the agent can carry on without it.
	var rpc rpcRequest
	_ = json.Unmarshal(body, &rpc)
	w.Header().Set("Content-Type", "application/json")
	if rpc.Method == "tools/call" && rpc.Params.Name == "msg_send" {
		if err := a.o.Spool.Put(it); err == nil {
			writeRPC(w, rpc.ID, map[string]any{"content": []map[string]string{{"type": "text",
				"text": "Queued: the core is unreachable right now. The message is delivered, in order, when it is back."}}})
			return
		}
	}
	writeRPCError(w, rpc.ID, "the core is unreachable right now; try again later")
}

func writeRPC(w io.Writer, id json.RawMessage, result any) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	_, _ = w.Write(b)
}

func writeRPCError(w io.Writer, id json.RawMessage, msg string) {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32000, "message": msg}})
	_, _ = w.Write(b)
}

// drainLoop sends held items whenever the core takes them again.
func (a *Agent) drainLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.o.Spool.Len() == 0 {
			continue
		}
		n := a.o.Spool.Drain(func(it Item) error {
			res, err := a.forward(ctx, it)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			// Delivered, but the core may still refuse it (a message to an agent that is gone):
			// say so in the log, since the agent was told it was queued.
			b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
			var rpc struct {
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
				Result struct {
					IsError bool `json:"isError"`
				} `json:"result"`
			}
			if it.Path == "/mcp" && json.Unmarshal(b, &rpc) == nil && (rpc.Error != nil || rpc.Result.IsError) {
				a.o.Logf("link: the core refused a held message: %s", strings.TrimSpace(string(b)))
			} else if res.StatusCode >= 400 {
				a.o.Logf("link: the core refused a held %s delivery: HTTP %d", it.Path, res.StatusCode)
			}
			return nil
		})
		if n > 0 {
			a.o.Logf("link: delivered %d held item(s)", n)
		}
	}
}

// wsURL turns the core's base URL into the link's ws(s) URL.
func wsURL(base string) string { return wsURLAt(base, Path) }

func wsURLAt(base, path string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		return "wss://" + strings.TrimPrefix(base, "https://") + path
	case strings.HasPrefix(base, "http://"):
		return "ws://" + strings.TrimPrefix(base, "http://") + path
	}
	return base + path
}

// linkLoop keeps the link up: dial, serve, and on loss back off (1 s doubling to 30 s, jittered).
func (a *Agent) linkLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := a.session(ctx)
		a.setConnected(false)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		a.o.Logf("link: %v; again in %s", err, backoff.Round(time.Second))
		wait := backoff/2 + time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (a *Agent) setConnected(v bool) {
	a.mu.Lock()
	a.connected = v
	a.mu.Unlock()
}

// session is one connection: hello, then status every few seconds and commands as they come.
func (a *Agent) session(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dctx, wsURL(a.o.CoreURL), &websocket.DialOptions{
		HTTPClient: a.client,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + a.o.CoreKey}},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	c.SetReadLimit(1 << 20)
	defer c.CloseNow()
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	var wmu sync.Mutex
	send := func(b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(sctx, 10*time.Second)
		defer cancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	hello, _ := Marshal(TypeHello, "", Hello{Agent: a.o.Agent, Placement: a.o.Placement, Engine: a.o.Engine, Version: a.o.Version, Buffered: a.o.Spool.Len()})
	if err := send(hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	a.setConnected(true)
	a.o.Logf("link: connected to %s", a.o.CoreURL)

	go func() {
		t := time.NewTicker(a.o.StatusEvery)
		defer t.Stop()
		pings := 0
		for {
			b, _ := Marshal(TypeStatus, "", a.status(sctx))
			if err := send(b); err != nil {
				stop()
				return
			}
			select {
			case <-sctx.Done():
				return
			case <-t.C:
			}
			if pings++; pings%2 == 0 {
				pctx, cancel := context.WithTimeout(sctx, 10*time.Second)
				err := c.Ping(pctx)
				cancel()
				if err != nil {
					stop()
					return
				}
			}
		}
	}()

	for {
		_, data, err := c.Read(sctx)
		if err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() == nil {
				return errors.New("connection lost")
			}
			return err
		}
		var env Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		go func(env Envelope) {
			res := Envelope{Type: TypeResult, Reply: env.ID}
			if err := a.handle(sctx, env); err != nil {
				res.Error = err.Error()
			}
			if env.ID != "" {
				b, _ := json.Marshal(res)
				_ = send(b)
			}
		}(env)
	}
}

func (a *Agent) handle(ctx context.Context, env Envelope) error {
	switch env.Type {
	case TypeWake:
		var w Wake
		if err := json.Unmarshal(env.Body, &w); err != nil {
			return err
		}
		req, err := a.o.Runtime.WakeRequest(ctx, a.o.AgentAPI, w.Input, w.Session, w.Idempotency)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+a.o.APIKey)
		res, err := a.client.Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return fmt.Errorf("wake: HTTP %d", res.StatusCode)
		}
		return nil
	case TypeNap:
		var n Nap
		_ = json.Unmarshal(env.Body, &n)
		if a.o.Nap == nil {
			return errors.New("this sidecar takes no naps")
		}
		if n.Reason == "" {
			n.Reason = "core"
		}
		return a.o.Nap(n.Reason)
	}
	return fmt.Errorf("unknown command %q", env.Type)
}

// status asks the agent's own engine API what it is doing.
func (a *Agent) status(ctx context.Context) Status {
	s := Status{State: "starting", Buffered: a.o.Spool.Len(), At: time.Now().UnixMilli()}
	if a.o.Runtime == nil || !a.o.Runtime.ValidAPIKey(a.o.APIKey) {
		return s
	}
	get := func(ctx context.Context, path string) (any, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.o.AgentAPI+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+a.o.APIKey)
		res, err := a.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode > 299 {
			return nil, nil
		}
		var v any
		if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&v); err != nil {
			return nil, err
		}
		return v, nil
	}
	actx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	act, state, err := a.o.Runtime.Activity(actx, get, a.rtState, time.Now())
	a.rtState = state
	if err == nil && act != nil {
		s.State, s.Activity = "running", act
	}
	return s
}
