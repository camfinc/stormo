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

// Call runs tool name with args (a JSON object; empty means {}).
func (c *Client) Call(ctx context.Context, name string, args json.RawMessage) (*Result, error) {
	if why := messaging.UntrustedWhy(ctx); why != "" && !messaging.ReadOnlyCoreTool(name) {
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
	raw, err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, false)
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
				if why := tools.UntrustedWhy(ctx, env); why != "" && !messaging.ReadOnlyCoreTool(name) {
					// a tainted run's core write (msg_send, fs_write, …): a person approves it
					return nil, messaging.NeedsApproval("core tool "+name, why)
				}
				res, err := c.Call(ctx, name, args)
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
				if strings.HasPrefix(text, "{") && json.Valid([]byte(text)) {
					return json.RawMessage(text), nil
				}
				return map[string]string{"result": text}, nil
			},
		})
	}
	return out
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
