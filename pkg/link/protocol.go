// Package link is the live wire between an agent and its core (docs/remote-core.md §1a, §4): one
// long-lived WebSocket the agent's sidecar dials, carrying status up and commands down, plus a
// relay on the agent's side that forwards its hooks and MCP calls to the core and buffers them
// while the core is away. The core end is pkg/core (LinkHub).
package link

import (
	"encoding/json"

	"github.com/camfinc/stormo/pkg/engine"
)

// Path is the core's link endpoint; agents authenticate with their core key.
const Path = "/link"

// RelayPort is where the sidecar's relay listens for the agent's hooks and MCP calls.
const RelayPort = 18690

// Envelope is every message on the wire. A command carries an ID; its answer is a "result" whose
// Reply is that ID, with Error when it failed.
type Envelope struct {
	ID    string          `json:"id,omitempty"`
	Type  string          `json:"type"`
	Reply string          `json:"reply,omitempty"`
	Error string          `json:"error,omitempty"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// Message types.
const (
	TypeHello  = "hello"  // agent → core, first message: Hello
	TypeStatus = "status" // agent → core, every few seconds: Status
	TypeResult = "result" // either way: the answer to a command
	TypeWake   = "wake"   // core → agent: Wake
	TypeNap    = "nap"    // core → agent: Nap
)

// Hello introduces the agent.
type Hello struct {
	Agent     string `json:"agent"`
	Placement string `json:"placement"`
	Engine    string `json:"engine"`
	Version   string `json:"version"`
	// Buffered is how many hook deliveries and queued messages wait in the agent's spool.
	Buffered int `json:"buffered"`
}

// Status is the agent's state as its sidecar sees it: its engine's API answering or not.
type Status struct {
	// running (its API answers) | starting (not yet)
	State    string           `json:"state"`
	Activity *engine.Activity `json:"activity,omitempty"`
	Buffered int              `json:"buffered"`
	At       int64            `json:"at"`
}

// Wake starts a turn on the agent's engine (engine.Runtime.WakeRequest).
type Wake struct {
	Input       string `json:"input"`
	Session     string `json:"session"`
	Idempotency string `json:"idempotency"`
}

// Nap asks the sidecar for a nap now.
type Nap struct {
	Reason string `json:"reason"`
}

// Marshal wraps a body in an envelope.
func Marshal(typ, id string, body any) ([]byte, error) {
	var raw json.RawMessage
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(Envelope{ID: id, Type: typ, Body: raw})
}
