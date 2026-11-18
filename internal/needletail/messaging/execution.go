package messaging

import (
	"context"
	"fmt"
	"sync/atomic"
)

// Execution kinds: a turn the core runs for a conversation of its own channel (docs/needletail.md,
// channel turns): a person's message (inbound) or the outcome of a request sent from it (return).
const (
	ExecInbound = "inbound"
	ExecReturn  = "return"
)

// Execution is a channel turn's identity, the core's: every core tool call made in it carries its id
// and an operation number (_meta.exec, _meta.op), so the core attributes what the turn did to the
// interaction that started it, even when the call reaches the core later (the relay's spool).
type Execution struct {
	ID, Kind string
	// OnAct records a tool call durably before it runs (the runtime's channel-turn store); an error
	// stops the call.
	OnAct func() error
	ops   atomic.Int64
	acted atomic.Int64
}

type executionKey struct{}

// WithExecution attaches the turn's execution; runtime-owned, never from model arguments.
func WithExecution(ctx context.Context, e *Execution) context.Context {
	return context.WithValue(ctx, executionKey{}, e)
}

// ExecutionOf is the turn's execution, nil outside a channel turn.
func ExecutionOf(ctx context.Context) *Execution {
	e, _ := ctx.Value(executionKey{}).(*Execution)
	return e
}

// NextOp is the next core call's operation identity: "<execution>/<n>". A turn never resumes after a
// restart (it ends interrupted), so an execution's numbers are never reused.
func (e *Execution) NextOp() string { return fmt.Sprintf("%s/%d", e.ID, e.ops.Add(1)) }

// Act records a tool call the turn is about to make, before it runs: a turn that acted is never run
// again (its side effects would repeat), and a crash after the call still shows it acted. An error
// (the record could not be kept) means the call must not run.
func (e *Execution) Act() error {
	if e.OnAct != nil {
		if err := e.OnAct(); err != nil {
			return err
		}
	}
	e.acted.Add(1)
	return nil
}

// Acted is how many tool calls the turn made.
func (e *Execution) Acted() int64 { return e.acted.Load() }
