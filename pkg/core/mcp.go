package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"slices"
	"sort"

	"github.com/camfinc/stormo/pkg/version"
)

// The core's MCP server (docs/core.md §4, §5): POST /mcp, Streamable HTTP without sessions or
// server-sent streams. Every request carries the agent's own core key, so a tool always knows
// which agent calls it. Hermes registers the tools as mcp_core_<name> (mcp_servers.core,
// pkg/engine/hermes). A tool's failure is a result with isError, so the model reads why.

// mcpVersions are the protocol versions the core answers in; an initialize asking for another
// gets the newest (the client then decides). All of them carry the same tools/* shapes.
var mcpVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// ToolCall is one tool invocation: who calls it and its arguments.
type ToolCall struct {
	Agent string
	Args  json.RawMessage
}

// MCPTool is a tool the core serves.
type MCPTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Run         func(ToolCall) (any, error)
}

// MCP serves tools over JSON-RPC; safe for concurrent use once built.
type MCP struct {
	keys  *AgentKeys
	tools map[string]MCPTool
	order []string
}

// NewMCP serves tools to agents holding a core key.
func NewMCP(keys *AgentKeys, tools ...[]MCPTool) *MCP {
	m := &MCP{keys: keys, tools: map[string]MCPTool{}}
	for _, set := range tools {
		for _, t := range set {
			m.tools[t.Name] = t
			m.order = append(m.order, t.Name)
		}
	}
	return m
}

// ToolError is a tool failure the agent should read (wrong argument, refused by a rule).
type ToolError struct{ Msg string }

func (e ToolError) Error() string { return e.Msg }

func toolErr(format string, a ...any) error { return ToolError{fmt.Sprintf(format, a...)} }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		body["error"] = e
	} else {
		body["result"] = result
	}
	writeJSONBody(w, 200, body)
}

// Handle serves /mcp.
func (m *MCP) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server-initiated stream and no session to delete; clients treat 405 as "not offered".
		w.Header().Set("Allow", "POST")
		writeJSONBody(w, 405, errBody("method_not_allowed", "POST JSON-RPC only"))
		return
	}
	agent, ok := m.keys.AgentFor(r.Header.Get("Authorization"))
	if !ok {
		if h := r.Header.Get("Authorization"); len(h) > 9 && h[7:9] == "${" {
			// Hermes passes an unset ${VAR} through literally: the agent's env lacks its key.
			log.Printf("mcp: an agent sent an unexpanded %s; is SWARM_CORE_KEY in its env file?", h[7:])
		}
		writeJSONBody(w, 401, errBody("unauthorized", "an agent core key is required"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		writeRPC(w, json.RawMessage("null"), nil, &rpcError{-32700, "request unreadable or larger than 1 MiB"})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		writeRPC(w, json.RawMessage("null"), nil, &rpcError{-32600, "one JSON-RPC request per POST"})
		return
	}
	if len(req.ID) == 0 {
		// A notification (notifications/initialized, cancelled, …): nothing to answer.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := mcpVersions[0]
		if slices.Contains(mcpVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "swarm-core", "version": version.String()},
			"instructions": "The swarm core: messages to other agents of this swarm (msg_*) and coordination of the shared " +
				"space (fs_*: locks, who changed a file). Your identity comes from your key.",
		}, nil)
	case "ping":
		writeRPC(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		tools := []map[string]any{}
		for _, n := range m.order {
			t := m.tools[n]
			tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
		}
		writeRPC(w, req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPC(w, req.ID, nil, &rpcError{-32602, "params must be {name, arguments}"})
			return
		}
		t, ok := m.tools[p.Name]
		if !ok {
			writeRPC(w, req.ID, nil, &rpcError{-32602, "unknown tool " + p.Name})
			return
		}
		if len(p.Arguments) == 0 || string(p.Arguments) == "null" {
			p.Arguments = json.RawMessage("{}")
		}
		out, err := t.Run(ToolCall{Agent: agent, Args: p.Arguments})
		if err != nil {
			msg := err.Error()
			if _, ok := err.(ToolError); !ok {
				log.Printf("mcp %s for %s: %v", p.Name, agent, err)
				msg = "the core could not do that (" + p.Name + "); see the core log"
			}
			writeRPC(w, req.ID, map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}, "isError": true}, nil)
			return
		}
		text, _ := json.MarshalIndent(out, "", " ")
		res := map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "isError": false}
		if obj, ok := toObject(out); ok {
			res["structuredContent"] = obj
		}
		writeRPC(w, req.ID, res, nil)
	default:
		writeRPC(w, req.ID, nil, &rpcError{-32601, "method not found: " + req.Method})
	}
}

// toObject is v as a JSON object, for structuredContent (which must be one).
func toObject(v any) (map[string]any, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var o map[string]any
	if json.Unmarshal(b, &o) != nil || o == nil {
		return nil, false
	}
	return o, true
}

// Tool names, sorted (for tests and docs).
func (m *MCP) Tools() []string {
	out := append([]string{}, m.order...)
	sort.Strings(out)
	return out
}

// decodeArgs reads a tool's arguments into v, refusing unknown fields so typos surface.
func decodeArgs(c ToolCall, v any) error {
	dec := json.NewDecoder(bytes.NewReader(c.Args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return toolErr("bad arguments: %v", err)
	}
	return nil
}

// schema builds a JSON Schema object with properties and required names.
func schema(required []string, props map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
func integer(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}
func boolean(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}
