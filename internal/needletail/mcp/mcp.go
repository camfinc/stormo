// Package mcp is the runtime's client of the swarm core's MCP server (docs/needletail.md §4.10):
// stateless JSON-RPC over POST with the agent's core key as a bearer token, the transport
// pkg/core/mcp.go serves. The core's tools are registered in the loop's registry as
// mcp_core_<name>, the names Hermes gives them, so prompts and wake text behave the same.
//
// The core is reached through the agent's sidecar relay (pkg/link), which answers for it while it
// is away: a msg_send is queued there, anything else fails fast with a JSON-RPC error. Neither
// ever fails a turn here: the model reads the failure as the tool's result.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// ProtocolVersion is the MCP version the client asks for.
const ProtocolVersion = "2025-06-18"

// Prefix is the registry name prefix of the core's tools (mcp_core_<name>).
const Prefix = "mcp_core_"

const (
	maxResponse = 4 << 20
	// CallTimeout bounds one request, as Hermes' mcp_servers.core timeout.
	CallTimeout = 60 * time.Second
)

// ErrAway is the core (or the relay standing in for it) not answering a request now: a transport
// error, a 5xx, or the relay's "unreachable" JSON-RPC error. Retrying later may work.
var ErrAway = errors.New("the core is unreachable right now")

// RPCError is a JSON-RPC error object the server answered with.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s (JSON-RPC %d)", e.Message, e.Code) }

// relayAway is the code pkg/link's relay answers with when it cannot reach the core.
const relayAway = -32000

// Away reports whether err means the core is away (worth retrying later).
func Away(err error) bool {
	var re *RPCError
	return errors.Is(err, ErrAway) || errors.As(err, &re) && re.Code == relayAway
}

// ToolInfo is one tool of tools/list.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client talks to one MCP server; safe for concurrent use.
type Client struct {
	URL  string
	Key  string
	HTTP *http.Client
	Log  func(string)
	// Internal is the agent's approvals.marked internal: a marked run also calls
	// messaging.InternalCoreTool without a person. Set once at boot.
	Internal bool

	ids         atomic.Int64
	mu          sync.Mutex
	initialized bool
	tools       []ToolInfo // tools/list, cached once it succeeded (per boot)
}

// New is a client of the server at url, authenticating with key.
func New(url, key string, log func(string)) *Client {
	return &Client{URL: url, Key: key, HTTP: &http.Client{Timeout: CallTimeout}, Log: log}
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

// rpc sends one request (a notification when notify) and returns its result.
func (c *Client) rpc(ctx context.Context, method string, params any, notify bool) (json.RawMessage, error) {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	var id int64
	if !notify {
		id = c.ids.Add(1)
		req["id"] = id
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Accept", "application/json, text/event-stream")
	hr.Header.Set("Authorization", "Bearer "+c.Key)
	if method != "initialize" {
		hr.Header.Set("Mcp-Protocol-Version", ProtocolVersion)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: CallTimeout}
	}
	res, err := client.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w (%v)", ErrAway, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w (%v)", ErrAway, err)
	case len(raw) > maxResponse:
		return nil, fmt.Errorf("%s: answer larger than %d bytes", method, maxResponse)
	case res.StatusCode >= 500:
		return nil, fmt.Errorf("%w (HTTP %d)", ErrAway, res.StatusCode)
	case res.StatusCode == http.StatusUnauthorized:
		return nil, errors.New("the core refused this agent's key (HTTP 401): is SWARM_CORE_KEY set?")
	case notify && res.StatusCode >= 200 && res.StatusCode < 300:
		return nil, nil
	case res.StatusCode < 200 || res.StatusCode > 299:
		return nil, fmt.Errorf("%s: HTTP %d", method, res.StatusCode)
	}
	if strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		raw = lastEvent(raw)
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: answer is not JSON-RPC", method)
	}
	if out.Error != nil {
		return nil, out.Error
	}
	return out.Result, nil
}

// lastEvent is the last data payload of a server-sent event stream (a server may answer a POST
// with one; the core answers JSON).
func lastEvent(raw []byte) []byte {
	var last []byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if d, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:")); ok {
			last = bytes.TrimSpace(d)
		}
	}
	return last
}

// initialize runs the handshake once per boot (the core keeps no session; it is still the
// protocol's first request).
func (c *Client) initialize(ctx context.Context) error {
	c.mu.Lock()
	done := c.initialized
	c.mu.Unlock()
	if done {
		return nil
	}
	_, err := c.rpc(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "needletail", "version": "1"},
	}, false)
	if err != nil {
		return err
	}
	if _, err := c.rpc(ctx, "notifications/initialized", nil, true); err != nil {
		return err
	}
	c.mu.Lock()
	c.initialized = true
	c.mu.Unlock()
	return nil
}

var toolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ListTools is the server's tools, asked once: a successful answer is kept for the process'
// life (the core's tool set changes only with its version).
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	c.mu.Lock()
	cached := c.tools
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	raw, err := c.rpc(ctx, "tools/list", map[string]any{}, false)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []ToolInfo `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tools/list: %v", err)
	}
	list := []ToolInfo{}
	for _, t := range out.Tools {
		if !toolName.MatchString(t.Name) {
			c.logf("mcp: skipping core tool with an unusable name %q", t.Name)
			continue
		}
		if len(t.InputSchema) == 0 || !json.Valid(t.InputSchema) {
			t.InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		list = append(list, t)
	}
	c.mu.Lock()
	c.tools = list
	c.mu.Unlock()
	return list, nil
}

// Result is a tools/call answer: its text, and whether the tool failed (isError).
type Result struct {
	Text    string
	IsError bool
}

// Call runs tool name with args (a JSON object; empty means {}), from a run marked as ctx says.
func (c *Client) Call(ctx context.Context, name string, args json.RawMessage) (*Result, error) {
	return c.CallMarked(ctx, name, args, messaging.UntrustedWhy(ctx))
}

// CallMarked runs tool name from a run that holds untrusted input for why ("" when it does not;
// the caller passes its conversation's mark too, which ctx alone may not carry): a marked run
// calls only what messaging.MarkedRunMay allows, and a message it sends carries why in the
// call's _meta, where the core keeps it (docs/core.md, marked messages).
func (c *Client) CallMarked(ctx context.Context, name string, args json.RawMessage, why string) (*Result, error) {
	if why != "" && !messaging.MarkedRunMay(name, c.Internal) {
		return nil, messaging.Deferred("core tool "+name, why)
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return nil, errors.New("arguments are not valid JSON")
	}
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	params := map[string]any{"name": name, "arguments": args}
	meta := map[string]string{}
	if why != "" && messaging.MarkCarryingCoreTool(name) {
		meta["untrusted"] = why
	}
	// a channel turn's calls carry its execution and an operation number: the core attributes
	// them to the interaction that started the turn, even when one reaches it later
	if e := messaging.ExecutionOf(ctx); e != nil {
		meta["exec"], meta["op"] = e.ID, e.NextOp()
	}
	if len(meta) > 0 {
		params["_meta"] = meta
	}
	raw, err := c.rpc(ctx, "tools/call", params, false)
	if err != nil {
		return nil, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("tools/call %s: %v", name, err)
	}
	parts := []string{}
	for _, p := range out.Content {
		if p.Type == "text" || p.Type == "" {
			parts = append(parts, p.Text)
		}
	}
	return &Result{Text: strings.Join(parts, "\n"), IsError: out.IsError}, nil
}

// Tools are the core's tools as registry entries named mcp_core_<name>. A tool error (isError)
// reaches the model as {"error": text}; so does the core being away.
func (c *Client) Tools(list []ToolInfo) []tools.Tool {
	out := make([]tools.Tool, 0, len(list))
	for _, info := range list {
		name := info.Name
		out = append(out, tools.Tool{
			Name:        Prefix + name,
			Description: info.Description,
			Schema:      info.InputSchema,
			Run: func(ctx context.Context, env *tools.Env, args json.RawMessage) (any, error) {
				why := tools.UntrustedWhy(ctx, env)
				if why != "" && !messaging.MarkedRunMay(name, c.Internal) {
					// a tainted run's core write that carries content out (fs_write, cap_run, …):
					// a person approves it; talk to another agent carries the mark instead, and
					// under approvals.marked internal a capability job or a WhatsApp send goes
					return nil, messaging.NeedsApproval("core tool "+name, why)
				}
				res, err := c.CallMarked(ctx, name, args, why)
				if err != nil {
					if Away(err) {
						return nil, fmt.Errorf("the swarm core is unreachable right now; try again later (%s)", name)
					}
					return nil, fmt.Errorf("the swarm core: %v", err)
				}
				if res.IsError {
					return nil, errors.New(strings.TrimSpace(res.Text))
				}
				text := strings.TrimSpace(res.Text)
				if mark := ReadMark(name, text); mark != "" {
					// a marked message's words reach the model only in a run marked by them
					if err := messaging.MarkUntrusted(ctx, mark); err != nil {
						return nil, fmt.Errorf("refused: the message carries untrusted input and this run cannot be marked (%v)", err)
					}
				}
				if strings.HasPrefix(text, "{") && json.Valid([]byte(text)) {
					return json.RawMessage(text), nil
				}
				return map[string]string{"result": text}, nil
			},
		})
	}
	return out
}

// ReadMark is why reading a core tool's answer marks the reader's run ("" when it does not): a
// swarm message msg_read or msg_thread shows whole, written by a run that held untrusted input
// (the core keeps its mark). msg_inbox lists such a message without its words, so it never marks.
func ReadMark(tool, text string) string {
	type msg struct {
		From      string `json:"from"`
		Untrusted string `json:"untrusted"`
	}
	var ms []msg
	switch tool {
	case "msg_read":
		var m msg
		if json.Unmarshal([]byte(text), &m) == nil {
			ms = []msg{m}
		}
	case "msg_thread":
		var t struct {
			Messages []msg `json:"messages"`
		}
		if json.Unmarshal([]byte(text), &t) == nil {
			ms = t.Messages
		}
	}
	for _, m := range ms {
		if why := strings.TrimSpace(m.Untrusted); why != "" {
			if r := []rune(why); len(r) > 200 {
				why = string(r[:200]) + "…"
			}
			return fmt.Sprintf("read a swarm message from %s that carries untrusted input (%s)", m.From, why)
		}
	}
	return ""
}

// Register adds the core's tools to reg as soon as the core lists them: it retries with backoff
// (the core or its relay may be away when the runtime boots) until it succeeds or ctx ends.
// Turns before that run without the core's tools.
func (c *Client) Register(ctx context.Context, reg *tools.Registry, minWait, maxWait time.Duration) {
	wait := minWait
	for attempt := 1; ; attempt++ {
		list, err := c.ListTools(ctx)
		if err == nil {
			reg.Add(c.Tools(list)...)
			c.logf("mcp: %d core tools registered", len(list))
			return
		}
		if attempt == 1 || attempt%10 == 0 {
			c.logf("mcp: core tools not available yet (attempt %d): %v", attempt, err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		wait = min(wait*2, maxWait)
	}
}

// Upload sends one file to a capability job of this agent that waits for it (the core's
// POST /v1/capabilities/jobs/<id>/files) and gives the job as the core answers it.
func (c *Client) Upload(ctx context.Context, job int64, name string, body []byte) (json.RawMessage, error) {
	base := strings.TrimSuffix(strings.TrimRight(c.URL, "/"), "/mcp")
	u := fmt.Sprintf("%s/v1/capabilities/jobs/%d/files?name=%s", base, job, url.QueryEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = int64(len(body))
	hc := &http.Client{Timeout: 10 * time.Minute} // a video is not a tool call: its own bound
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the core: %w", err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		if json.Unmarshal(out, &e) == nil && e.Error.Message != "" {
			return nil, fmt.Errorf("the core refused it: %s", e.Error.Message)
		}
		return nil, fmt.Errorf("the core answered HTTP %d", res.StatusCode)
	}
	return out, nil
}
