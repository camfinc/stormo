package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/camfinc/stormo/pkg/link"
)

// LinkHub is the core's end of the agents' links (pkg/link, docs/remote-core.md §1a): who is
// connected, their latest status, and commands sent down with their answers. An agent with a link
// is reachable wherever it runs; without one the core falls back to its local paths (compose, the
// agent's local API port).
type LinkHub struct {
	mu    sync.Mutex
	conns map[string]*linkConn
	// last is each agent's last link state, kept after it drops (for "lost since").
	last map[string]LinkInfo
	db   *DB
}

// LinkInfo is an agent's link as /api/fleet shows it.
type LinkInfo struct {
	// connected | lost
	State     string `json:"state"`
	Placement string `json:"placement,omitempty"`
	Since     int64  `json:"since"`
	LastSeen  int64  `json:"lastSeen"`
	// Buffered: hook deliveries and queued messages waiting in the agent's spool.
	Buffered int `json:"buffered"`
}

type linkConn struct {
	agent   string
	c       *websocket.Conn
	wmu     sync.Mutex
	status  *link.Status
	info    LinkInfo
	pending map[string]chan link.Envelope
	pmu     sync.Mutex
}

// NewLinkHub makes an empty hub.
func NewLinkHub(db *DB) *LinkHub {
	return &LinkHub{conns: map[string]*linkConn{}, last: map[string]LinkInfo{}, db: db}
}

// Handle accepts an agent's link (authenticated by its core key, already resolved to agent).
func (h *LinkHub) Handle(w http.ResponseWriter, r *http.Request, agent string) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	c.SetReadLimit(1 << 20)
	ctx := r.Context()
	hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, data, err := c.Read(hctx)
	cancel()
	var env link.Envelope
	var hello link.Hello
	if err != nil || json.Unmarshal(data, &env) != nil || env.Type != link.TypeHello || json.Unmarshal(env.Body, &hello) != nil || hello.Agent != agent {
		c.Close(websocket.StatusPolicyViolation, "first message is hello, for the key's own agent")
		return
	}
	now := time.Now().UnixMilli()
	lc := &linkConn{agent: agent, c: c, pending: map[string]chan link.Envelope{},
		info: LinkInfo{State: "connected", Placement: hello.Placement, Since: now, LastSeen: now, Buffered: hello.Buffered}}
	h.mu.Lock()
	if old := h.conns[agent]; old != nil {
		old.c.Close(websocket.StatusGoingAway, "replaced by a newer link")
	}
	h.conns[agent] = lc
	h.mu.Unlock()
	h.db.Audit("agent:"+agent, "link.connect", agent, hello.Placement)

	defer func() {
		h.mu.Lock()
		if h.conns[agent] == lc {
			delete(h.conns, agent)
			info := lc.snapshot()
			info.State, info.Since = "lost", time.Now().UnixMilli()
			h.last[agent] = info
		}
		h.mu.Unlock()
		lc.failPending()
		c.CloseNow()
	}()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var env link.Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		lc.pmu.Lock()
		lc.info.LastSeen = time.Now().UnixMilli()
		lc.pmu.Unlock()
		switch env.Type {
		case link.TypeStatus:
			var s link.Status
			if json.Unmarshal(env.Body, &s) == nil {
				lc.pmu.Lock()
				lc.status, lc.info.Buffered = &s, s.Buffered
				lc.pmu.Unlock()
			}
		case link.TypeResult:
			lc.pmu.Lock()
			ch := lc.pending[env.Reply]
			delete(lc.pending, env.Reply)
			lc.pmu.Unlock()
			if ch != nil {
				ch <- env
			}
		}
	}
}

func (lc *linkConn) snapshot() LinkInfo {
	lc.pmu.Lock()
	defer lc.pmu.Unlock()
	return lc.info
}

func (lc *linkConn) failPending() {
	lc.pmu.Lock()
	defer lc.pmu.Unlock()
	for id, ch := range lc.pending {
		ch <- link.Envelope{Type: link.TypeResult, Reply: id, Error: "link lost"}
		delete(lc.pending, id)
	}
}

// Close drops every link (the core is stopping; http.Server.Close does not reach hijacked ones).
func (h *LinkHub) Close() {
	h.mu.Lock()
	conns := make([]*linkConn, 0, len(h.conns))
	for _, lc := range h.conns {
		conns = append(conns, lc)
	}
	h.mu.Unlock()
	for _, lc := range conns {
		lc.c.Close(websocket.StatusGoingAway, "core stopping")
	}
}

// Connected reports whether the agent has a live link.
func (h *LinkHub) Connected(agent string) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conns[agent] != nil
}

// Info is the agent's link state (connected, or lost since), nil when it never linked.
func (h *LinkHub) Info(agent string) *LinkInfo {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if lc := h.conns[agent]; lc != nil {
		i := lc.snapshot()
		return &i
	}
	if i, ok := h.last[agent]; ok {
		return &i
	}
	return nil
}

// Status is the agent's latest status over its link, nil without a live link or when stale (30 s).
func (h *LinkHub) Status(agent string) *link.Status {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	lc := h.conns[agent]
	h.mu.Unlock()
	if lc == nil {
		return nil
	}
	lc.pmu.Lock()
	defer lc.pmu.Unlock()
	if lc.status == nil || time.Since(time.UnixMilli(lc.status.At)) > 30*time.Second {
		return nil
	}
	s := *lc.status
	return &s
}

// ErrNoLink is a command to an agent without a live link.
var ErrNoLink = errors.New("no live link")

// Command sends a command down an agent's link and waits for its answer.
func (h *LinkHub) Command(ctx context.Context, agent, typ string, body any) error {
	if h == nil {
		return ErrNoLink
	}
	h.mu.Lock()
	lc := h.conns[agent]
	h.mu.Unlock()
	if lc == nil {
		return ErrNoLink
	}
	idb := make([]byte, 8)
	_, _ = rand.Read(idb)
	id := hex.EncodeToString(idb)
	msg, err := link.Marshal(typ, id, body)
	if err != nil {
		return err
	}
	ch := make(chan link.Envelope, 1)
	lc.pmu.Lock()
	lc.pending[id] = ch
	lc.pmu.Unlock()
	lc.wmu.Lock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = lc.c.Write(wctx, websocket.MessageText, msg)
	cancel()
	lc.wmu.Unlock()
	if err != nil {
		lc.pmu.Lock()
		delete(lc.pending, id)
		lc.pmu.Unlock()
		return fmt.Errorf("link to %s: %w", agent, err)
	}
	select {
	case res := <-ch:
		if res.Error != "" {
			return fmt.Errorf("%s: %s", agent, res.Error)
		}
		return nil
	case <-ctx.Done():
		lc.pmu.Lock()
		delete(lc.pending, id)
		lc.pmu.Unlock()
		return ctx.Err()
	}
}

// WatchLost raises a link_lost finding for an agent whose link has been gone longer than after.
func (h *LinkHub) WatchLost(stop <-chan struct{}, every, after time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		h.mu.Lock()
		lost := map[string]LinkInfo{}
		for id, i := range h.last {
			if h.conns[id] == nil && time.Since(time.UnixMilli(i.Since)) > after {
				lost[id] = i
			}
		}
		h.mu.Unlock()
		for id, i := range lost {
			_ = h.db.RaiseFinding(id, "link_lost", "medium",
				fmt.Sprintf("no link from %s since %s", or(i.Placement, "its sidecar"), time.UnixMilli(i.Since).UTC().Format(time.RFC3339)), time.Now().UnixMilli())
		}
	}
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
