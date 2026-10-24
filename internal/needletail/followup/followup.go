// Package followup is the agent's one-time follow-ups (docs/needletail.md §4.12): a message to
// send, or a turn to take, once, later ("say hola in 10 seconds", "remind me tomorrow at 9").
// Recurring work is a scheduled job (sched); a follow-up is not a job: agent.yaml has no form for
// it, it is never proposed for review, and it lives in the runtime's private state
// (state/needletail/followups.json, 0600; napped as Raw, since what it says is client data, and
// restored at boot) so a pending one survives a restart.
//
// A follow-up belongs to the conversation it was made in, as the runtime knows it (the session's
// root, source and memory audience: never the model's words) and, unless it names another target
// the messaging policy allows, is delivered there: a Slack thread or DM through the gateway, an
// API conversation (the owner's app chat) by an assistant row in its transcript. A prompt
// follow-up is a turn in that conversation, in the scheduled runs' lane.
//
// The queue fires each one when it is due (a timer armed for the next one, re-armed on every
// change), once; one missed while the runtime was down fires at the next boot, marked late. A
// failure is retried with backoff, up to MaxAttempts, then kept as failed (for list) and logged.
package followup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// Kinds of follow-ups.
const (
	// KindMessage is a fixed text, sent as it is (no model turn).
	KindMessage = "message"
	// KindPrompt is a turn the agent takes at the due time, in the conversation.
	KindPrompt = "prompt"
)

// Statuses.
const (
	StatusPending = "pending"
	StatusFailed  = "failed"
)

// TargetOrigin is a follow-up's default target: the conversation it was made in.
const TargetOrigin = "origin"

// Bounds.
const (
	// MinDelay is the shortest follow-up (Options.MinDelay lowers it for tests only).
	MinDelay = 5 * time.Second
	// MaxAhead is the furthest a follow-up may be due.
	MaxAhead = 30 * 24 * time.Hour
	// MaxPerConversation and MaxPerAgent bound the pending follow-ups.
	MaxPerConversation = 20
	MaxPerAgent        = 200
	// MaxByHandlers and MaxByHandlersPerAudience bound the pending follow-ups the agent's Luau
	// handlers and tools made (By luau:…): a handler runs on every message, in every thread.
	MaxByHandlers            = 20
	MaxByHandlersPerAudience = 5
	// MaxText bounds a text or a prompt (bytes).
	MaxText = 4 << 10
	// MaxAttempts is how many times a failing follow-up is tried before it is kept as failed.
	MaxAttempts = 3
	// File is the queue's file in the home; Version its "v".
	File    = "state/needletail/followups.json"
	Version = 1
)

// Internal bounds.
const (
	keepFailed = 7 * 24 * time.Hour // failed follow-ups are listed this long
	maxFailed  = 50                 // and at most this many
	maxFiring  = 4                  // follow-ups delivered at once
	maxFile    = 8 << 20
)

// Waits (variables for the tests).
var (
	// backoff is the wait after a failed attempt (the first, the second).
	backoff = []time.Duration{30 * time.Second, 2 * time.Minute}
	// busyRetry: a busy conversation or lane is asked again this often, for busyFor before it
	// counts as a failed attempt.
	busyRetry = 5 * time.Second
	busyFor   = 10 * time.Minute
	// recheck: with nothing due sooner, the file is read again this often (a oneshot beside
	// serve may have written it).
	recheck = 30 * time.Second
)

// Conversation is the conversation a follow-up was made in, as the runtime knows it: the session's
// root, its source (api_server, slack, cli, cron) and the memory audience of its turns (operator,
// dm:<person>, channel:<channel>, or none). Operator: the owner's own turn (a oneshot), which
// lists and cancels every conversation's follow-ups.
type Conversation struct {
	Session  string `json:"session,omitempty"`
	Source   string `json:"source,omitempty"`
	Audience string `json:"audience,omitempty"`
	// Thread is the DM thread it was asked in (a Slack DM's conversation is the whole DM: its
	// follow-up is posted back in that thread).
	Thread string `json:"thread,omitempty"`
	// Users are who asked: the authors of the message the run handled (as the gateway heard
	// them), whose admission is checked again when it fires.
	Users    []string `json:"users,omitempty"`
	Operator bool     `json:"-"`
}

// Item is one follow-up as followups.json v1 keeps it.
type Item struct {
	ID string `json:"id"`
	// Created and Due are RFC 3339, UTC, to the millisecond.
	Created string       `json:"created"`
	Due     string       `json:"due"`
	Kind    string       `json:"kind"`
	Text    string       `json:"text,omitempty"`
	Prompt  string       `json:"prompt,omitempty"`
	Origin  Conversation `json:"origin"`
	// Target is TargetOrigin or a target in send_message's grammar.
	Target string `json:"target"`
	// By is who made it: "model" (the follow_up tool) or "luau:<tool>".
	By string `json:"by,omitempty"`
	// Untrusted: made by a run handling input overheard in a channel the agent listens to (only
	// a text back to its own conversation can be).
	Untrusted bool   `json:"untrusted,omitempty"`
	Status    string `json:"status"`
	Attempts  int    `json:"attempts"`
	LastError string `json:"last_error,omitempty"`
	// NextTry, when set, is when a retry is due (after a failure or a busy conversation).
	NextTry string `json:"next_try,omitempty"`
	// Late: it was due while the runtime was down, and fired at the next boot.
	Late     bool   `json:"late,omitempty"`
	FailedAt string `json:"failed_at,omitempty"`
}

type file struct {
	V     int    `json:"v"`
	Items []Item `json:"followups"`
}

func parse(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// stamp is a time as the file keeps it: RFC 3339 in UTC, to the millisecond.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z07:00") }

// DueAt is when the follow-up is due.
func (it Item) DueAt() time.Time { return parse(it.Due) }

// when is when it is to be tried next.
func (it Item) when() time.Time {
	if it.NextTry != "" {
		return parse(it.NextTry)
	}
	return parse(it.Due)
}

// Errors a Firer answers.
var (
	// ErrBusy: the conversation runs a turn, or the scheduled runs' lane is full: asked again
	// shortly (not an attempt), for up to 10 minutes.
	ErrBusy = errors.New("busy")
	// ErrInterrupted: the runtime is stopping; the follow-up stays pending for the next boot.
	ErrInterrupted = errors.New("interrupted: the runtime is stopping")
	// ErrGone: its conversation no longer exists; kept as failed at once.
	ErrGone = errors.New("the conversation it belongs to no longer exists")
	// ErrRefused: the follow-up may not be delivered (what made it may not, now or ever); kept
	// as failed at once.
	ErrRefused = errors.New("refused")
)

// Errors of the queue.
var ErrNotFound = errors.New("no such follow-up in this conversation (follow_up list shows them)")

// Firer delivers a due follow-up: a message to its target, or a prompt's turn and its reply.
type Firer interface {
	Fire(ctx context.Context, it Item) error
}

// Options configure a Queue.
type Options struct {
	// Home is the agent's home (File, and the temp dir fsx.TmpDir).
	Home string
	// Location is the agent's wall clock (at times are read in it, results shown in it).
	Location *time.Location
	Now      func() time.Time
	Log      func(string)
	// Redact masks secret values in what is logged and kept (last_error).
	Redact func(string) string
	// Check refuses a text holding a secret value the runtime knows (tools.SecretCheck).
	Check func(string) error
	// ScanPrompt is the scan a scheduled job's prompt meets (sched.ScanPrompt): a prompt it
	// blocks is refused, with its reason ("" passes).
	ScanPrompt func(prompt string) string
	// Origin is the conversation a session belongs to, from the runtime's own records.
	Origin func(session string) Conversation
	// MinDelay overrides MinDelay (tests only; 0: MinDelay).
	MinDelay time.Duration
}

// Queue is the agent's follow-ups: created by the tool and Luau, fired by its timer once Start
// gives it a Firer. Safe for concurrent use.
type Queue struct {
	o         Options
	path, tmp string

	mu       sync.Mutex
	items    []Item
	gen      string
	loaded   bool
	readOnly error // a file this runtime cannot write (a newer version, damaged)
	firing   map[string]bool
	busy     map[string]time.Time // when a follow-up first found its conversation busy
	firer    Firer
	check    func(target string, origin Conversation) error
	stopping bool

	kick   chan struct{}
	wg     sync.WaitGroup // the loop and the firings
	runCtx context.Context
	cancel context.CancelFunc
	stop   context.CancelFunc // ends the loop
}

// New builds a queue; Start fires its follow-ups.
func New(o Options) *Queue {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.Redact == nil {
		o.Redact = func(s string) string { return s }
	}
	if o.MinDelay <= 0 {
		o.MinDelay = MinDelay
	}
	q := &Queue{o: o, path: filepath.Join(o.Home, filepath.FromSlash(File)), tmp: filepath.Join(o.Home, fsx.TmpDir),
		firing: map[string]bool{}, busy: map[string]time.Time{}, kick: make(chan struct{}, 1)}
	q.runCtx, q.cancel = context.WithCancel(context.Background())
	return q
}

func (q *Queue) logf(format string, args ...any) {
	if q.o.Log != nil {
		q.o.Log(q.o.Redact(fmt.Sprintf(format, args...)))
	}
}

// SetTargetCheck is the messaging policy for a follow-up's target (the gateway's): target in
// send_message's grammar, or TargetOrigin for a Slack conversation's own. nil: only an API
// conversation can be followed up.
func (q *Queue) SetTargetCheck(check func(target string, origin Conversation) error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.check = check
}

func (q *Queue) poke() {
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

// ---- the file ---------------------------------------------------------------------------------

func fileGen(p string) string {
	st, err := os.Stat(p)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d/%d", st.ModTime().UnixNano(), st.Size())
}

// load reads the file when it changed since it was last read or written (callers hold mu).
func (q *Queue) load() error {
	gen := fileGen(q.path)
	if q.loaded && gen == q.gen {
		return q.readOnly
	}
	q.items, q.gen, q.loaded, q.readOnly = nil, gen, true, nil
	if gen == "" {
		return nil
	}
	body, err := fsx.ReadFileBelow(q.o.Home, File, maxFile)
	if err != nil {
		q.readOnly = fmt.Errorf("%s: %v", File, err)
		return q.readOnly
	}
	var f file
	if err := json.Unmarshal(body, &f); err != nil {
		q.readOnly = fmt.Errorf("%s is damaged: %v", File, err)
		return q.readOnly
	}
	if f.V != Version {
		q.readOnly = fmt.Errorf("%s is version %d; this runtime reads %d", File, f.V, Version)
		return q.readOnly
	}
	q.items = f.Items
	return nil
}

// save writes the file whole, through the home's temp dir, 0600 (callers hold mu).
func (q *Queue) save() error {
	if q.readOnly != nil {
		return q.readOnly
	}
	q.prune()
	items := q.items
	if items == nil {
		items = []Item{}
	}
	body, err := json.Marshal(file{V: Version, Items: items})
	if err != nil {
		return err
	}
	if err := fsx.WriteAtomicIn(q.path, q.tmp, append(body, '\n'), 0o600); err != nil {
		return err
	}
	q.gen = fileGen(q.path)
	return nil
}

// prune drops failed follow-ups older than keepFailed, and past the newest maxFailed.
func (q *Queue) prune() {
	now := q.o.Now()
	var failed []int
	kept := q.items[:0:0]
	for _, it := range q.items {
		if it.Status == StatusFailed && now.Sub(parse(it.FailedAt)) > keepFailed {
			continue
		}
		kept = append(kept, it)
	}
	for i, it := range kept {
		if it.Status == StatusFailed {
			failed = append(failed, i)
		}
	}
	if len(failed) > maxFailed {
		drop := map[int]bool{}
		sort.Slice(failed, func(a, b int) bool { return kept[failed[a]].FailedAt < kept[failed[b]].FailedAt })
		for _, i := range failed[:len(failed)-maxFailed] {
			drop[i] = true
		}
		out := kept[:0:0]
		for i, it := range kept {
			if !drop[i] {
				out = append(out, it)
			}
		}
		kept = out
	}
	q.items = kept
}

func (q *Queue) find(id string) int {
	for i, it := range q.items {
		if it.ID == id {
			return i
		}
	}
	return -1
}

// newID is fu + the time in milliseconds (hex) + 4 random hex digits: unique, sortable, no client
// text (callers hold mu).
func (q *Queue) newID(now time.Time) string {
	for {
		b := make([]byte, 2)
		_, _ = rand.Read(b)
		id := fmt.Sprintf("fu%x%s", now.UnixMilli(), hex.EncodeToString(b))
		if q.find(id) < 0 {
			return id
		}
	}
}

// ---- the timer --------------------------------------------------------------------------------

// Start loads the queue, marks what fell due while the runtime was down as late, and fires each
// follow-up when it is due through firer, until Stop.
func (q *Queue) Start(firer Firer) {
	q.mu.Lock()
	q.firer = firer
	if err := q.load(); err != nil {
		q.logf("follow-ups: %v: none will fire", err)
	}
	now := q.o.Now()
	late := 0
	for i := range q.items {
		if it := &q.items[i]; it.Status == StatusPending && !it.when().After(now) && !it.Late {
			it.Late = true
			late++
		}
	}
	if late > 0 {
		if err := q.save(); err != nil {
			q.logf("follow-ups: %v", err)
		}
		q.logf("follow-ups: %d fell due while the agent was down; they fire now, marked late", late)
	}
	q.mu.Unlock()
	ctx, stop := context.WithCancel(context.Background())
	q.stop = stop
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		q.loop(ctx)
	}()
}

// loop fires what is due, then sleeps until the next follow-up is due (at most recheck, so a
// file another process wrote is read), a change (poke) or Stop.
func (q *Queue) loop(ctx context.Context) {
	t := time.NewTimer(time.Hour)
	defer t.Stop()
	for {
		wait := q.dispatch()
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-q.kick:
		case <-t.C:
		}
	}
}

// dispatch starts the due follow-ups (at most maxFiring at once) and answers how long until the
// next one is due.
func (q *Queue) dispatch() time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopping || q.firer == nil {
		return recheck
	}
	if err := q.load(); err != nil {
		return recheck
	}
	now := q.o.Now()
	wait := recheck
	for _, it := range q.items {
		if it.Status != StatusPending || q.firing[it.ID] {
			continue
		}
		at := it.when()
		if at.After(now) {
			wait = min(wait, at.Sub(now))
			continue
		}
		if len(q.firing) >= maxFiring {
			continue // the next firing's end pokes the loop
		}
		q.firing[it.ID] = true
		q.wg.Add(1)
		go q.fire(it)
	}
	return max(wait, 10*time.Millisecond)
}

type firingKey struct{}

// Firing is the follow-up whose delivery ctx belongs to ("" outside one): a follow-up's own turn
// may not schedule another (a chain that never ends).
func Firing(ctx context.Context) string {
	s, _ := ctx.Value(firingKey{}).(string)
	return s
}

func (q *Queue) fire(it Item) {
	defer q.wg.Done()
	ctx := context.WithValue(q.runCtx, firingKey{}, it.ID)
	late := ""
	if it.Late {
		late = ", late"
	}
	q.logf("follow-up %s fires (%s to %s, conversation %s, attempt %d%s)", it.ID, it.Kind, it.Target, orNone(it.Origin.Session), it.Attempts+1, late)
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("the delivery crashed: %v", p)
			}
		}()
		return q.firer.Fire(ctx, it)
	}()
	q.settle(it, err)
	q.poke()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// settle records how a firing went: delivered (removed), interrupted (left as it was), busy
// (asked again shortly), failed (retried with backoff, then kept as failed).
func (q *Queue) settle(fired Item, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.firing, fired.ID)
	if lerr := q.load(); lerr != nil {
		q.logf("follow-up %s: %v", fired.ID, lerr)
		return
	}
	i := q.find(fired.ID)
	if i < 0 {
		return // removed meanwhile (another process)
	}
	it := &q.items[i]
	now := q.o.Now()
	switch {
	case err == nil:
		q.items = append(q.items[:i], q.items[i+1:]...)
		delete(q.busy, fired.ID)
		q.logf("follow-up %s delivered (%s, %s late)", fired.ID, fired.Kind, now.Sub(fired.DueAt()).Round(time.Second))
	case q.stopping || errors.Is(err, ErrInterrupted) || errors.Is(err, context.Canceled) && q.runCtx.Err() != nil:
		q.logf("follow-up %s interrupted by the shutdown: it fires at the next boot", fired.ID)
		return
	case errors.Is(err, ErrBusy) && q.busySince(fired.ID, now).After(now.Add(-busyFor)):
		it.NextTry = stamp(now.Add(busyRetry))
	default:
		if errors.Is(err, ErrBusy) {
			err = fmt.Errorf("the conversation (or the scheduled runs' lane) was busy for %s", busyFor)
		}
		delete(q.busy, fired.ID)
		it.Attempts++
		it.LastError = clip(q.o.Redact(err.Error()), 300)
		if it.Attempts >= MaxAttempts || errors.Is(err, ErrGone) || errors.Is(err, ErrRefused) {
			it.Status, it.FailedAt, it.NextTry = StatusFailed, stamp(now), ""
			q.logf("follow-up %s failed after %d attempts, dropped (kept for list): %s", fired.ID, it.Attempts, it.LastError)
		} else {
			wait := backoff[min(it.Attempts, len(backoff))-1]
			it.NextTry = stamp(now.Add(wait))
			q.logf("follow-up %s failed (attempt %d of %d), tried again in %s: %s", fired.ID, it.Attempts, MaxAttempts, wait, it.LastError)
		}
	}
	if serr := q.save(); serr != nil {
		q.logf("follow-up %s: %v", fired.ID, serr)
	}
}

// busySince is when the follow-up first found its conversation busy in this run (callers hold mu).
func (q *Queue) busySince(id string, now time.Time) time.Time {
	t, ok := q.busy[id]
	if !ok {
		q.busy[id], t = now, now
		q.logf("follow-up %s: its conversation is busy; asked again every %s", id, busyRetry)
	}
	return t
}

// Stop starts no firing from here on (the runtime's shutdown has begun); a firing cut short
// is interrupted, not an attempt.
func (q *Queue) Stop() {
	q.mu.Lock()
	q.stopping = true
	q.mu.Unlock()
	if q.stop != nil {
		q.stop()
	}
}

// Shutdown stops the queue and waits for the firings until ctx ends, then cancels them.
func (q *Queue) Shutdown(ctx context.Context) {
	q.Stop()
	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		q.cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			q.logf("follow-ups: a delivery did not stop in time; not waiting for it")
		}
	}
	q.cancel()
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
