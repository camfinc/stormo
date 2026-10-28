// Package hooks posts the agent's activity to the swarm core (docs/needletail.md §4.11): one
// wire.Hook v1 per event, POSTed to {ingest_url}/needletail and signed with the agent's core key
// (X-Needletail-Signature: sha256=<hex HMAC-SHA256(body, key)>), so the core's office shows the
// agent's tool calls live and attributes its shared-space writes.
//
// Delivery never blocks a turn: events go into a bounded queue (the oldest is dropped when it is
// full) that one worker sends in order. A transport error or a 5xx is retried with backoff, with
// the same body and delivery id (the core drops a repeat); a 4xx is dropped (the core refuses
// those for good). An event older than MaxAge is dropped: the core refuses anything more than
// ten minutes off. Tool inputs are redacted and cut short; no event is sent for model calls.
package hooks

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Kind is the engine's segment of the core's ingest route (POST {ingest_url}/needletail).
const Kind = "needletail"

// Statuses of tool_done and session_end events.
const (
	StatusOK        = "ok"
	StatusError     = "error"
	StatusCancelled = "cancelled"
)

// Bounds on what an event carries and how it is delivered.
const (
	maxString    = 512      // bytes of one string value in a tool input
	maxKeys      = 32       // keys of one object
	maxItems     = 16       // items of one array
	maxDepth     = 4        // nesting kept
	maxInput     = 32 << 10 // an input larger than this once encoded keeps only its path and command
	timeout      = 5 * time.Second
	defaultQueue = 256
)

// pathKeys and command are what the core reads from an input (pkg/core monitor.go): they survive
// when an input is too large to send whole.
var pathKeys = []string{"path", "file_path", "filepath", "target", "filename", "command"}

// Options configure a Sender.
type Options struct {
	// URL is the core's ingest URL (the sidecar relay's /ingest); deliveries go to URL/needletail.
	URL string
	// Key is the agent's core key (SWARM_CORE_KEY): the HMAC key.
	Key string
	// Redact masks secret values in tool inputs.
	Redact func(string) string
	// Session gives a session's conversation (its root: compactions move the tip, the conversation
	// stays) and platform (the session's source: slack, api_server, cron, …).
	Session func(session string) (conversation, platform string)
	Log     func(string)
	HTTP    *http.Client
	// Queue bounds the events waiting (default 256).
	Queue int
	// MaxAge is how long an event is retried (default 9 min, under the core's 10 min skew).
	MaxAge time.Duration
	// Wait is the first retry's delay (default 1 s), doubling up to MaxWait (default 30 s).
	Wait, MaxWait time.Duration
	Now           func() time.Time
}

type item struct {
	body []byte
	sig  string
	at   time.Time
	what string // event and tool, for logs
}

// Sender queues and delivers hooks; it is the loop's Observer. Safe for concurrent use.
type Sender struct {
	o Options

	mu      sync.Mutex
	queue   []item
	sending bool
	dropped int
	refused int // the worker's alone
	signal  chan struct{}
	idle    chan struct{} // closed and replaced when the queue empties
	done    chan struct{}
}

// New builds a sender; Start runs its worker.
func New(o Options) *Sender {
	if o.Redact == nil {
		o.Redact = func(s string) string { return s }
	}
	if o.Session == nil {
		o.Session = func(s string) (string, string) { return s, "" }
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: timeout}
	}
	if o.Queue <= 0 {
		o.Queue = defaultQueue
	}
	if o.MaxAge <= 0 {
		o.MaxAge = 9 * time.Minute
	}
	if o.Wait <= 0 {
		o.Wait = time.Second
	}
	if o.MaxWait <= 0 {
		o.MaxWait = 30 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.URL = strings.TrimSuffix(o.URL, "/") + "/" + Kind
	return &Sender{o: o, signal: make(chan struct{}, 1), idle: make(chan struct{}), done: make(chan struct{})}
}

func (s *Sender) logf(format string, args ...any) {
	if s.o.Log != nil {
		s.o.Log(s.o.Redact(fmt.Sprintf(format, args...)))
	}
}

// Start runs the worker until ctx ends.
func (s *Sender) Start(ctx context.Context) { go s.run(ctx) }

// Done is closed once the worker stopped.
func (s *Sender) Done() <-chan struct{} { return s.done }

// Flush waits until every queued event was delivered or dropped, or ctx ends (shutdown gives the
// last session_end a moment).
func (s *Sender) Flush(ctx context.Context) {
	for {
		s.mu.Lock()
		empty := len(s.queue) == 0 && !s.sending
		idle := s.idle
		s.mu.Unlock()
		if empty {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-idle:
		case <-s.done:
			return
		}
	}
}

// Sign is the signature header's value for body.
func Sign(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// NewDelivery is a UUIDv7: unique per event, time-ordered.
func NewDelivery(now time.Time) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(now.UnixMilli()))
	copy(b[0:6], ms[2:8])
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// emit builds, signs and queues one event; it never blocks.
func (s *Sender) emit(h wire.Hook) {
	now := s.o.Now()
	h.V = wire.HookVersion
	h.Delivery = NewDelivery(now)
	h.At = now.UTC().Format(time.RFC3339Nano)
	if h.Session != "" {
		h.Session, h.Platform = s.o.Session(h.Session)
	}
	if h.Input == nil {
		h.Input = map[string]any{}
	}
	body, err := json.Marshal(h)
	if err != nil {
		s.logf("hooks: %s not encoded: %v", h.Event, err)
		return
	}
	it := item{body: body, sig: Sign(s.o.Key, body), at: now, what: strings.TrimSpace(h.Event + " " + h.Tool)}
	s.mu.Lock()
	dropped := 0
	if len(s.queue) >= s.o.Queue {
		s.queue = s.queue[1:]
		s.dropped++
		dropped = s.dropped
	}
	s.queue = append(s.queue, it)
	s.mu.Unlock()
	if dropped == 1 || dropped%100 == 0 && dropped > 0 {
		s.logf("hooks: queue full (%d); dropped the oldest event (%d so far)", s.o.Queue, dropped)
	}
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

func (s *Sender) next() (item, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		s.sending = false
		close(s.idle)
		s.idle = make(chan struct{})
		return item{}, false
	}
	it := s.queue[0]
	s.queue = s.queue[1:]
	s.sending = true
	return it, true
}

func (s *Sender) run(ctx context.Context) {
	defer close(s.done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.signal:
		}
		for {
			it, ok := s.next()
			if !ok {
				break
			}
			if !s.deliver(ctx, it) {
				return
			}
		}
	}
}

// errRefused is a 4xx: the core will never take this delivery.
var errRefused = errors.New("refused")

// deliver sends one event until it is taken, refused or too old; false when ctx ended.
func (s *Sender) deliver(ctx context.Context, it item) bool {
	wait := s.o.Wait
	for attempt := 1; ; attempt++ {
		err := s.post(ctx, it)
		switch {
		case err == nil:
			s.refused = 0 // a later refusal starts a new run, logged at once
			return true
		case ctx.Err() != nil:
			return false
		case errors.Is(err, errRefused):
			// The first refusal and every 100th: a core refusing everything (a wrong key) would
			// otherwise log every tool call.
			s.refused++
			if s.refused == 1 || s.refused%100 == 0 {
				s.logf("hooks: the core refused %s: %v (%d refused so far)", it.what, err, s.refused)
			}
			return true
		case s.o.Now().Sub(it.at) > s.o.MaxAge:
			s.logf("hooks: %s dropped after %d attempts: %v", it.what, attempt, err)
			return true
		}
		if attempt == 1 {
			s.logf("hooks: %s not delivered, retrying: %v", it.what, err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
		wait = min(wait*2, s.o.MaxWait)
	}
}

func (s *Sender) post(ctx context.Context, it item) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, s.o.URL, bytes.NewReader(it.body))
	if err != nil {
		return fmt.Errorf("%w: %v", errRefused, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(wire.HookSignatureHeader, it.sig)
	res, err := s.o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	case res.StatusCode >= 400 && res.StatusCode < 500:
		return fmt.Errorf("%w: %s", errRefused, strings.TrimSpace(fmt.Sprintf("HTTP %d %s", res.StatusCode, strings.TrimSpace(string(msg)))))
	}
	return fmt.Errorf("HTTP %d", res.StatusCode)
}

// ---- the loop's observer ---------------------------------------------------------------------

// TurnStart is a turn beginning on session: session_start.
func (s *Sender) TurnStart(session string) {
	s.emit(wire.Hook{Event: wire.EventSessionStart, Session: session})
}

// TurnEnd is the turn ending (session is its tip): session_end, with how it ended.
func (s *Sender) TurnEnd(session string, err error) {
	status := StatusOK
	switch {
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		status = StatusCancelled
	case err != nil:
		status = StatusError
	}
	s.emit(wire.Hook{Event: wire.EventSessionEnd, Session: session, Status: status})
}

// ToolStart is tool_start, with the call's real id and its (redacted, shortened) arguments.
func (s *Sender) ToolStart(session string, c wire.ToolCall) {
	s.emit(wire.Hook{Event: wire.EventToolStart, Session: session, Tool: c.Name, CallID: c.ID, Input: s.Input(c.Arguments)})
}

// ToolDone is tool_done: status and duration, and the arguments again (the core reads a file
// tool's path from either).
func (s *Sender) ToolDone(session string, c wire.ToolCall, _ string, failed bool, took time.Duration) {
	status := StatusOK
	if failed {
		status = StatusError
	}
	s.emit(wire.Hook{Event: wire.EventToolDone, Session: session, Tool: c.Name, CallID: c.ID, Input: s.Input(c.Arguments),
		Status: status, DurationMS: took.Milliseconds()})
}

// Approval is approval_waiting (a request put to the approvers) or approval_answered (decided,
// expired, run): the request's id and status only, never its payload, so the core and the review
// can count approvals.
func (s *Sender) Approval(event, session, id, status string) {
	s.emit(wire.Hook{Event: event, Session: session, ApprovalID: id, Status: status})
}

// Text is not sent: an assistant's words are client data and no hook carries them.
func (s *Sender) Text(string, string) {}

// Input is a tool call's arguments as a hook carries them: every string redacted then cut to 512
// bytes, objects and arrays bounded; when still too large, only the path and command keys.
func (s *Sender) Input(args string) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || m == nil {
		return map[string]any{}
	}
	out, _ := s.clean(m, 0).(map[string]any)
	if b, err := json.Marshal(out); err == nil && len(b) <= maxInput {
		return out
	}
	small := map[string]any{}
	for _, k := range pathKeys {
		if v, ok := out[k].(string); ok {
			small[k] = v
		}
	}
	return small
}

func (s *Sender) clean(v any, depth int) any {
	switch x := v.(type) {
	case string:
		return clip(s.o.Redact(x), maxString)
	case map[string]any:
		if depth >= maxDepth {
			return map[string]any{}
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// The keys the core reads come first, so the bound never cuts them.
		sort.Slice(keys, func(i, j int) bool {
			pi, pj := slices.Contains(pathKeys, keys[i]), slices.Contains(pathKeys, keys[j])
			if pi != pj {
				return pi
			}
			return keys[i] < keys[j]
		})
		out := map[string]any{}
		for i, k := range keys {
			if i == maxKeys {
				break
			}
			out[clip(s.o.Redact(k), 64)] = s.clean(x[k], depth+1)
		}
		return out
	case []any:
		if depth >= maxDepth {
			return []any{}
		}
		out := []any{}
		for i, e := range x {
			if i == maxItems {
				break
			}
			out = append(out, s.clean(e, depth+1))
		}
		return out
	}
	return v // numbers, booleans, null
}

// clip cuts s to at most n bytes on a rune boundary, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
