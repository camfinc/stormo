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
	TypeOrder  = "order"  // core → runner: Order (its answer's body is an OrderResult)
)

// RunnerPath is where a runner dials the core (a person's token with the operate scope; its
// hello's Agent and Placement name the placement it serves).
const RunnerPath = "/runner"

// Order is a runner's work: a lifecycle action (start, stop, restart, status) on an agent, or
// "capability": a capability's container run for an agent (Job set).
type Order struct {
	Action string `json:"action"`
	Agent  string `json:"agent"`
	Job    *Job   `json:"job,omitempty"`
}

// Job is a capability container to run: the input JSON on stdin, its JSON answer on stdout.
// Secrets are env var names the runner fills from its own secrets file.
type Job struct {
	ID             int64           `json:"id"`
	Capability     string          `json:"capability"`
	Image          string          `json:"image"`
	Command        []string        `json:"command,omitempty"`
	Secrets        []string        `json:"secrets,omitempty"`
	Input          json.RawMessage `json:"input"`
	TimeoutMinutes int             `json:"timeoutMinutes"`
	// Service set: the runner keeps the capability's service container (booted on demand, asleep when
	// idle) and posts the job to it instead of running a one-shot container.
	Service *ServiceJob `json:"service,omitempty"`
}

// ServiceJob is how a runner runs a service capability; Env holds plain settings only (secrets come
// from the runner's own file, by name). Template: a bundled template the runner builds the image of.
type ServiceJob struct {
	Template    string            `json:"template,omitempty"`
	Port        int               `json:"port"`
	Health      string            `json:"health"`
	Env         map[string]string `json:"env,omitempty"`
	MemoryMB    int               `json:"memoryMB,omitempty"`
	ShmMB       int               `json:"shmMB,omitempty"`
	CPUs        float64           `json:"cpus,omitempty"`
	IdleMinutes int               `json:"idleMinutes"`
	BootMinutes int               `json:"bootMinutes"`
}

// OrderResult is what a runner did: its log lines, the agent's state after, a job's output.
type OrderResult struct {
	Log    []string        `json:"log"`
	State  string          `json:"state,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
}

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
