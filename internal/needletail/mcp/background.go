package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// Background makes tool calls off the turn (the automatic fs_note after a file tool's shared
// write): a bounded queue, one worker, each call retried while the core is away. A full queue drops
// the oldest call; a call the core refuses is logged and dropped.
type Background struct {
	c   *Client
	max int
	// MaxAge is how long a call is retried (default 10 min); Wait the first retry's delay.
	MaxAge, Wait, MaxWait time.Duration

	mu     sync.Mutex
	queue  []bgCall
	signal chan struct{}
	done   chan struct{}
}

type bgCall struct {
	name string
	args json.RawMessage
	at   time.Time
}

// Background starts a worker for calls in the background; it stops when ctx ends.
func (c *Client) Background(ctx context.Context, size int) *Background {
	b := &Background{c: c, max: max(size, 1), MaxAge: 10 * time.Minute, Wait: 5 * time.Second, MaxWait: time.Minute,
		signal: make(chan struct{}, 1), done: make(chan struct{})}
	go b.run(ctx)
	return b
}

// Go queues one call; it never blocks.
func (b *Background) Go(name string, args any) {
	raw, err := json.Marshal(args)
	if err != nil {
		return
	}
	b.mu.Lock()
	if len(b.queue) >= b.max {
		b.c.logf("mcp: background queue full; dropped a %s call", b.queue[0].name)
		b.queue = b.queue[1:]
	}
	b.queue = append(b.queue, bgCall{name: name, args: raw, at: time.Now()})
	b.mu.Unlock()
	select {
	case b.signal <- struct{}{}:
	default:
	}
}

// Done is closed once the worker stopped.
func (b *Background) Done() <-chan struct{} { return b.done }

func (b *Background) next() (bgCall, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.queue) == 0 {
		return bgCall{}, false
	}
	c := b.queue[0]
	b.queue = b.queue[1:]
	return c, true
}

func (b *Background) run(ctx context.Context) {
	defer close(b.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.signal:
		}
		for {
			call, ok := b.next()
			if !ok {
				break
			}
			if !b.send(ctx, call) {
				return
			}
		}
	}
}

// send makes one call, retrying while the core is away; false when ctx ended.
func (b *Background) send(ctx context.Context, call bgCall) bool {
	wait := b.Wait
	for {
		cctx, cancel := context.WithTimeout(ctx, CallTimeout)
		res, err := b.c.Call(cctx, call.name, call.args)
		cancel()
		switch {
		case ctx.Err() != nil:
			return false
		case err == nil && res.IsError:
			b.c.logf("mcp: %s refused: %s", call.name, res.Text)
			return true
		case err == nil:
			return true
		case !Away(err):
			b.c.logf("mcp: %s failed: %v", call.name, err)
			return true
		case time.Since(call.at) > b.MaxAge:
			b.c.logf("mcp: %s dropped after %s: %v", call.name, b.MaxAge, err)
			return true
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
		wait = min(wait*2, b.MaxWait)
	}
}
