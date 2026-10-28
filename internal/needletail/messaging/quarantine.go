package messaging

import (
	"context"
	"fmt"
	"sync"
)

type quarantineKey struct{}
type quarantineState struct {
	mu  sync.Mutex
	why string
}
type quarantine struct {
	state   *quarantineState
	persist func(string) error
}

// WithQuarantine shares a monotonic run mark with child calls. A nonnil persistence
// callback binds this context to its conversation; nil preserves an existing binding.
func WithQuarantine(ctx context.Context, persist func(string) error) context.Context {
	if q, ok := ctx.Value(quarantineKey{}).(*quarantine); ok {
		if persist == nil {
			return ctx
		}
		return context.WithValue(ctx, quarantineKey{}, &quarantine{state: q.state, persist: persist})
	}
	return context.WithValue(ctx, quarantineKey{}, &quarantine{state: &quarantineState{}, persist: persist})
}

// MarkUntrusted must succeed before marked data is returned. Even when persistence
// fails, the in-memory mark stays set so later operations cannot write as trusted.
func MarkUntrusted(ctx context.Context, why string) error {
	q, ok := ctx.Value(quarantineKey{}).(*quarantine)
	if !ok {
		return fmt.Errorf("quarantine provenance is unavailable")
	}
	if why == "" {
		why = UntrustedConversation
	}
	q.state.mu.Lock()
	if q.state.why == "" {
		q.state.why = why
	}
	q.state.mu.Unlock()
	if q.persist != nil {
		return q.persist(why)
	}
	return nil
}

// WithSession attaches the runtime-owned origin; model arguments cannot override it.
type sessionKey struct{}

func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionKey{}, id)
}
func SessionOf(ctx context.Context) string { id, _ := ctx.Value(sessionKey{}).(string); return id }

// ShareQuarantine carries provenance into a narrower host-call context without
// replacing its deadline, cancellation or other values.
func ShareQuarantine(ctx, from context.Context) context.Context {
	if q, ok := from.Value(quarantineKey{}).(*quarantine); ok {
		return context.WithValue(ctx, quarantineKey{}, q)
	}
	return ctx
}
