// Package approvals is the runtime's human approvals (docs/needletail.md §7, "Approvals"): an
// action a tainted run may not take on its own (send to another conversation, a core write, a
// persistent change) or a risky command (approvals.commands) is put to a person instead of
// refused. The runtime renders a card from the structured action (never the model's text), sends
// it to the approvers' Slack DMs (never into the tainted conversation), and on Approve runs
// exactly the action whose hash the card carries, once. Deny, a timeout and a restart past the
// due time deny. The model only ever gets "pending approval <id>"; the outcome is posted back to
// the conversation and kept for its next turn.
//
// State: state/needletail/approvals-v1.json (pending and recent requests, with their payloads:
// Raw in snapshots) and state/needletail/approvals-audit.jsonl (append-only: ids, hashes,
// decisions, approver ids, times; never payload text: State).
package approvals

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Kinds of action.
const (
	// KindTool is a call of one of the agent's tools (a built-in, the gateway's send_message, a
	// core tool mcp_core_<name>): Tool and its exact Args.
	KindTool = "tool"
	// KindDelivery is a tainted scheduled run's held report: Delivery.
	KindDelivery = "delivery"
)

// Statuses of a request.
const (
	StatusPending  = "pending"
	StatusApproved = "approved" // decided, running
	StatusDone     = "done"     // approved and run
	StatusFailed   = "failed"   // approved, but running it failed
	StatusDenied   = "denied"
	StatusExpired  = "expired" // no decision in time: denied
)

// Action is what a person approves: the structured action the runtime runs, never text the model
// wrote about it.
type Action struct {
	Kind     string          `json:"kind"`
	Tool     string          `json:"tool,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
	Delivery *Delivery       `json:"delivery,omitempty"`
}

// Delivery is a held scheduled report: the exact text and where its job delivers.
type Delivery struct {
	JobID   string   `json:"job_id"`
	JobName string   `json:"job_name,omitempty"`
	Session string   `json:"session,omitempty"`
	Text    string   `json:"text"`
	Targets []string `json:"targets"`
	Trusted bool     `json:"trusted,omitempty"`
	Failure bool     `json:"failure,omitempty"`
}

// Hash is the action's identity: sha256 over its kind, tool, compacted arguments and delivery,
// and the conversation it was asked in. A card carries it; only that hash runs.
func (a Action) Hash(conversation string) string {
	h := sha256.New()
	args := a.Args
	if c, err := compact(args); len(args) > 0 && err == nil {
		args = json.RawMessage(c)
	}
	d := []byte("null")
	if a.Delivery != nil {
		d, _ = json.Marshal(a.Delivery)
	}
	fmt.Fprintf(h, "needletail-approval v1\n%s\n%s\n%d:%s\n%d:%s\n%s", a.Kind, a.Tool, len(args), args, len(d), d, conversation)
	return hex.EncodeToString(h.Sum(nil))
}

func compact(raw json.RawMessage) (string, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return "", err
	}
	return b.String(), nil
}

// Origin is where a request's outcome is told: the conversation's Slack thread or DM, or none
// (an API conversation keeps it as a row in its transcript; a scheduled run has none).
type Origin struct {
	Channel string `json:"channel,omitempty"`
	Thread  string `json:"thread,omitempty"`
}

// Card is a request as the approvers see it, rendered by the runtime from the action.
type Card struct {
	// Title is one line: what the agent wants to do.
	Title string `json:"title"`
	// Target is where it acts (a Slack target, a path, a job), "" when none.
	Target string `json:"target,omitempty"`
	// Payload is exactly what runs: the full text, or a diff for a file, skill, script or job;
	// PayloadKind says which ("text", "diff", "json", "command").
	Payload     string `json:"payload"`
	PayloadKind string `json:"payload_kind"`
}

// Request is one approval request (approvals-v1.json).
type Request struct {
	ID     string `json:"id"`
	Hash   string `json:"hash"`
	Action Action `json:"action"`
	// Conversation is the conversation (root session) it was asked in; Session the session the
	// call ran in ("" for a scheduled report's).
	Conversation string `json:"conversation,omitempty"`
	Session      string `json:"session,omitempty"`
	Origin       Origin `json:"origin"`
	Card         Card   `json:"card"`
	// Why is why it needs a person (the run's taint, the risky-command rule).
	Why string `json:"why"`
	// Reason is what the agent said before the call: its claim, shown as such.
	Reason string `json:"reason,omitempty"`
	// Taint is who and what introduced the conversation's taint (none for a risky command in a
	// trusted conversation); no taint author may approve.
	Taint []wire.Taint `json:"taint,omitempty"`
	// Trust: the card offers "Approve & trust thread" (endorse the conversation too).
	Trust   bool      `json:"trust,omitempty"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`
	Status  string    `json:"status"`
	// DecidedBy, DecidedAt and Trusted: who decided, when, and whether they trusted the thread.
	DecidedBy string    `json:"decided_by,omitempty"`
	DecidedAt time.Time `json:"decided_at,omitzero"`
	Trusted   bool      `json:"trusted,omitempty"`
	// Result is what running it answered (bounded), or why it failed.
	Result string `json:"result,omitempty"`
	// Cards are the messages the card was posted as (one per approver DM), updated in place.
	Cards []Posted `json:"cards,omitempty"`
	// Noted: the outcome is in the conversation (a row its next turn reads).
	Noted bool `json:"noted,omitempty"`
}

// Posted is a posted card.
type Posted struct {
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// Pending is whether no decision has been taken.
func (r *Request) Pending() bool { return r.Status == StatusPending }

// Final is whether nothing more happens to it.
func (r *Request) Final() bool {
	return r.Status != StatusPending && r.Status != StatusApproved
}

// Notifier shows requests to people: the Slack gateway.
type Notifier interface {
	// Ask posts the card to approver's DM; it returns where.
	Ask(ctx context.Context, r *Request, approver string) (Posted, error)
	// Update rewrites a posted card with the request's outcome (buttons gone).
	Update(ctx context.Context, r *Request, p Posted) error
	// Tell posts the outcome in the request's origin conversation (a runtime notice).
	Tell(ctx context.Context, r *Request) error
}

// Errors.
var (
	ErrUnavailable = errors.New("approvals are not available")
	ErrCap         = errors.New("too many approval requests")
	ErrNotFound    = errors.New("no such approval request")
	ErrDecided     = errors.New("this request was already decided")
	ErrHash        = errors.New("the card does not match the request (payload changed)")
	ErrApprover    = errors.New("you are not an approver for this agent")
	ErrAuthor      = errors.New("you introduced this conversation's untrusted input, so you cannot approve its actions")
	ErrTooLarge    = errors.New("the payload is too large to show whole on a card")
)

// MaxPayload is the most payload bytes a card shows (split over its blocks): a payload a person
// cannot see whole is never put to them.
const MaxPayload = 100 << 10

// Defaults.
const (
	DefaultTimeout         = 30 * time.Minute
	DefaultDeliveryExpiry  = 3 * 24 * time.Hour
	DefaultPerConversation = 3
	DefaultPerDay          = 20
	keepDecided            = 7 * 24 * time.Hour
	maxEntries             = 1000
	maxResult              = 2000
)

// Executor runs an approved action under ctx (endorsed: messaging.Endorsed): its result.
type Executor func(ctx context.Context, r *Request) (string, error)

// Options configure a Service.
type Options struct {
	// StateDir is state/needletail; TmpDir the home's temp dir (atomic writes).
	StateDir, TmpDir string
	// Approvers are the Slack user ids who may decide (approvals.approvers, within allowed_users).
	Approvers []string
	// Timeout is how long a request waits (default 30 min); DeliveryExpiry a held report's.
	Timeout, DeliveryExpiry time.Duration
	// PerConversation is the most pending requests of one conversation (default 3), PerDay the
	// most requests of the agent in 24 h (default 20).
	PerConversation, PerDay int
	// Execute runs an approved action of each kind.
	Execute map[string]Executor
	// Trust endorses a conversation (sessions.Store.Trust), by an approver.
	Trust func(conversation, by string) error
	// Note puts a request's outcome in its conversation as a row the next turn reads; an error
	// (the conversation busy) leaves it for the turn's start (Notes).
	Note func(r *Request, text string) error
	// Hook is told requests and decisions (the core's approval_waiting / approval_answered).
	Hook func(event string, r *Request)
	Now  func() time.Time
	Log  func(string)
}

// Service holds the requests. Safe for concurrent use; serve is the store's single writer.
type Service struct {
	o        Options
	mu       sync.Mutex
	notifier Notifier
	loaded   bool
	file     *storeFile
	// busy are requests whose decision is being carried out (outside the lock).
	busy map[string]bool
}

type storeFile struct {
	Version  int                 `json:"version"`
	Requests map[string]*Request `json:"requests"`
}

// New builds a service; Boot loads its store.
func New(o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.DeliveryExpiry <= 0 {
		o.DeliveryExpiry = DefaultDeliveryExpiry
	}
	if o.PerConversation <= 0 {
		o.PerConversation = DefaultPerConversation
	}
	if o.PerDay <= 0 {
		o.PerDay = DefaultPerDay
	}
	return &Service{o: o, busy: map[string]bool{}}
}

func (s *Service) logf(format string, args ...any) {
	if s.o.Log != nil {
		s.o.Log(fmt.Sprintf("approvals: "+format, args...))
	}
}

// SetNotifier gives the service the gateway that shows cards (nil: approvals unavailable).
func (s *Service) SetNotifier(n Notifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifier = n
}

// Available is "" when requests can be made, else why not.
func (s *Service) Available() string {
	if s == nil {
		return "approvals are not set up"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.notifier == nil:
		return "approvals need the Slack gateway, which is not running"
	case len(s.o.Approvers) == 0:
		return "no approver is configured (approvals.approvers, within allowed_users)"
	}
	return ""
}

// Approvers are who may decide.
func (s *Service) Approvers() []string { return append([]string{}, s.o.Approvers...) }

// Timeout is how long a request waits for a decision.
func (s *Service) Timeout() time.Duration { return s.o.Timeout }

func (s *Service) path() string      { return filepath.Join(s.o.StateDir, "approvals-v1.json") }
func (s *Service) auditPath() string { return filepath.Join(s.o.StateDir, "approvals-audit.jsonl") }

// load reads the store once (s.mu held). A file of another version, or one it cannot read, makes
// approvals unavailable rather than overwriting it.
func (s *Service) load() error {
	if s.loaded {
		return nil
	}
	f := &storeFile{Version: 1, Requests: map[string]*Request{}}
	b, err := os.ReadFile(s.path())
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return err
	default:
		if err := json.Unmarshal(b, f); err != nil {
			return fmt.Errorf("approvals-v1.json: %w", err)
		}
		if f.Version != 1 {
			return fmt.Errorf("approvals-v1.json: version %d is not one this runtime reads", f.Version)
		}
		if f.Requests == nil {
			f.Requests = map[string]*Request{}
		}
		for id, r := range f.Requests {
			if r == nil || r.ID != id {
				return fmt.Errorf("approvals-v1.json: a request is not keyed by its id")
			}
		}
	}
	s.file, s.loaded = f, true
	return nil
}

func (s *Service) save() error {
	b, err := json.Marshal(s.file)
	if err != nil {
		return err
	}
	return fsx.WriteAtomicIn(s.path(), s.o.TmpDir, b, 0o600)
}

// auditEntry is one line of approvals-audit.jsonl: ids, hashes, ids of people and times only.
type auditEntry struct {
	At           string `json:"at"`
	Event        string `json:"event"`
	ID           string `json:"id"`
	Hash         string `json:"hash,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Tool         string `json:"tool,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	By           string `json:"by,omitempty"`
	Trusted      bool   `json:"trusted,omitempty"`
}

// audit appends one line (0600, fsynced); a failure is logged, never fatal.
func (s *Service) audit(event string, r *Request, by string) {
	e := auditEntry{At: s.o.Now().UTC().Format(time.RFC3339Nano), Event: event, By: by}
	if r != nil {
		e.ID, e.Hash, e.Kind, e.Tool, e.Conversation, e.Trusted = r.ID, r.Hash, r.Action.Kind, r.Action.Tool, r.Conversation, r.Trusted && event == StatusApproved
	}
	AppendAudit(s.auditPath(), e, s.logf)
}

// Audit records an endorsement made outside a request (the owner trusting a conversation from
// the app): event "trusted".
func (s *Service) Audit(event, conversation, by string) {
	AppendAudit(s.auditPath(), auditEntry{At: s.o.Now().UTC().Format(time.RFC3339Nano), Event: event, Conversation: conversation, By: by}, s.logf)
}

// AppendAudit appends e to the audit file at path.
func AppendAudit(path string, e any, logf func(string, ...any)) {
	line, err := json.Marshal(e)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
			_, err = f.Write(append(line, '\n'))
			if err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil && logf != nil {
		logf("audit record not written: %v", err)
	}
}

// Ask is a request to make.
type Ask struct {
	Action       Action
	Conversation string
	Session      string
	Origin       Origin
	Card         Card
	Why          string
	Reason       string
	Taint        []wire.Taint
	Trust        bool
}

// Request puts an action to the approvers: it is kept, its card posted to each approver's DM, and
// returned pending. Refused (nothing kept) when approvals are unavailable, over the caps, or with
// a payload too large to show.
func (s *Service) Request(ctx context.Context, a Ask) (*Request, error) {
	if why := s.Available(); why != "" {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, why)
	}
	if len(a.Card.Payload) > MaxPayload {
		return nil, ErrTooLarge
	}
	now := s.o.Now()
	expiry := s.o.Timeout
	if a.Action.Kind == KindDelivery {
		expiry = s.o.DeliveryExpiry
	}
	r := &Request{ID: newID(now), Hash: a.Action.Hash(a.Conversation), Action: a.Action, Conversation: a.Conversation, Session: a.Session,
		Origin: a.Origin, Card: a.Card, Why: a.Why, Reason: clip(strings.TrimSpace(a.Reason), 1500), Taint: a.Taint, Trust: a.Trust && a.Conversation != "",
		Created: now, Expires: now.Add(expiry), Status: StatusPending}
	s.Expire(ctx)
	s.mu.Lock()
	if err := s.load(); err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	pending, today := 0, 0
	key := capKey(r)
	for _, x := range s.file.Requests {
		if x.Pending() && capKey(x) == key {
			pending++
		}
		if now.Sub(x.Created) < 24*time.Hour {
			today++
		}
	}
	switch {
	case pending >= s.o.PerConversation:
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %d are already waiting in this conversation (at most %d): wait for the owner to decide them", ErrCap, pending, s.o.PerConversation)
	case today >= s.o.PerDay:
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %d in the last 24 hours (at most %d)", ErrCap, today, s.o.PerDay)
	}
	s.prune(now)
	s.file.Requests[r.ID] = r
	if err := s.save(); err != nil {
		delete(s.file.Requests, r.ID)
		s.mu.Unlock()
		return nil, fmt.Errorf("the approval request could not be kept: %w", err)
	}
	n := s.notifier
	approvers := append([]string{}, s.o.Approvers...)
	snap := r.copy()
	s.mu.Unlock()
	s.audit("requested", snap, "")

	posted := []Posted{}
	var last error
	for _, who := range approvers {
		if slices.ContainsFunc(snap.Taint, func(t wire.Taint) bool { return slices.Contains(t.Users, who) }) {
			continue // a taint author never gets the card
		}
		p, err := n.Ask(ctx, snap, who)
		if err != nil {
			last = err
			continue
		}
		posted = append(posted, p)
	}
	s.mu.Lock()
	r.Cards = posted
	if len(posted) == 0 {
		r.Status, r.Result = StatusFailed, "the card could not be posted"
		_ = s.save()
		snap = r.copy()
		s.mu.Unlock()
		s.audit(StatusFailed, snap, "")
		if last == nil {
			last = errors.New("no approver may decide it (every approver introduced this conversation's taint)")
		}
		return nil, fmt.Errorf("%w: the request could not be shown to an approver: %v", ErrUnavailable, last)
	}
	err := s.save()
	snap = r.copy()
	s.mu.Unlock()
	if err != nil {
		s.logf("%s: card refs not kept: %v", snap.ID, err)
	}
	if s.o.Hook != nil {
		s.o.Hook(wire.EventApprovalWaiting, snap)
	}
	s.logf("%s: %s waits for a decision until %s (%s)", snap.ID, snap.Card.Title, snap.Expires.UTC().Format(time.RFC3339), snap.Hash[:12])
	return snap, nil
}

// capKey is what the per-conversation cap counts by: the conversation, or a held report's job.
func capKey(r *Request) string {
	if r.Conversation != "" {
		return r.Conversation
	}
	if r.Action.Delivery != nil {
		return "job:" + r.Action.Delivery.JobID
	}
	return "session:" + r.Session
}

// Decision is a person's answer to a card.
type Decision struct {
	ID, Hash string
	// By is the approver's Slack user id, as the authenticated socket gave it.
	By      string
	Approve bool
	// Trust: "Approve & trust thread".
	Trust bool
}

// Decide takes a person's decision: checked (the request pending, the hash the card's, the person
// an approver and no taint author), recorded before anything runs (so a second click or a replay
// finds it decided), then carried out: an approval runs exactly the action (endorsed), a trust
// endorses the conversation; the cards are updated and the outcome told in the conversation.
func (s *Service) Decide(ctx context.Context, d Decision) (*Request, error) {
	s.Expire(ctx)
	now := s.o.Now()
	s.mu.Lock()
	if err := s.load(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	r := s.file.Requests[d.ID]
	switch {
	case r == nil:
		s.mu.Unlock()
		return nil, ErrNotFound
	case !slices.Contains(s.o.Approvers, d.By):
		snap := r.copy()
		s.mu.Unlock()
		s.audit("refused_approver", snap, d.By)
		return nil, ErrApprover
	case slices.ContainsFunc(r.Taint, func(t wire.Taint) bool { return slices.Contains(t.Users, d.By) }):
		snap := r.copy()
		s.mu.Unlock()
		s.audit("refused_author", snap, d.By)
		return nil, ErrAuthor
	case !r.Pending():
		snap := r.copy()
		s.mu.Unlock()
		return snap, fmt.Errorf("%w (%s)", ErrDecided, snap.Status)
	case d.Hash != r.Hash || r.Hash != r.Action.Hash(r.Conversation):
		snap := r.copy()
		s.mu.Unlock()
		s.audit("refused_hash", snap, d.By)
		return nil, ErrHash
	}
	r.DecidedBy, r.DecidedAt = d.By, now
	if d.Approve {
		r.Status = StatusApproved
		r.Trusted = d.Trust && r.Trust
	} else {
		r.Status = StatusDenied
	}
	if err := s.save(); err != nil {
		r.Status, r.DecidedBy, r.DecidedAt, r.Trusted = StatusPending, "", time.Time{}, false
		s.mu.Unlock()
		return nil, fmt.Errorf("the decision could not be kept: %w", err)
	}
	s.busy[r.ID] = true
	snap := r.copy()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.busy, snap.ID)
		s.mu.Unlock()
	}()
	s.audit(snap.Status, snap, d.By)
	if snap.Status == StatusApproved {
		snap = s.carryOut(ctx, snap)
	}
	s.finish(ctx, snap)
	return snap, nil
}

// carryOut runs an approved request (a snapshot): the trust first (so the action runs in an
// endorsed conversation too), then exactly the action, endorsed. It returns the request as kept.
func (s *Service) carryOut(ctx context.Context, r *Request) *Request {
	trusted := r.Trusted
	if trusted && s.o.Trust != nil {
		if err := s.o.Trust(r.Conversation, r.DecidedBy); err != nil {
			s.logf("%s: the conversation could not be trusted: %v", r.ID, err)
			trusted = false
		} else {
			s.audit("trusted", r, r.DecidedBy)
		}
	}
	status, result := StatusDone, ""
	ex := s.o.Execute[r.Action.Kind]
	if ex == nil {
		status, result = StatusFailed, "the runtime cannot run a "+r.Action.Kind
	} else if r.Hash != r.Action.Hash(r.Conversation) {
		status, result = StatusFailed, "the stored action no longer matches its hash: not run"
	} else {
		out, err := ex(messaging.WithEndorsed(ctx, r.ID), r)
		result = out
		if err != nil {
			status, result = StatusFailed, err.Error()
		}
	}
	s.mu.Lock()
	out := r
	if stored := s.file.Requests[r.ID]; stored != nil {
		stored.Status, stored.Result, stored.Trusted = status, clip(result, maxResult), trusted
		if err := s.save(); err != nil {
			s.logf("%s: outcome not kept: %v", r.ID, err)
		}
		out = stored.copy()
	}
	s.mu.Unlock()
	s.audit(status, out, "")
	return out
}

// finish updates the cards of a decided request (a snapshot), tells its conversation and hooks
// the decision.
func (s *Service) finish(ctx context.Context, r *Request) {
	s.mu.Lock()
	n := s.notifier
	s.mu.Unlock()
	if n != nil {
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		for _, p := range r.Cards {
			if err := n.Update(uctx, r, p); err != nil {
				s.logf("%s: card not updated: %v", r.ID, err)
			}
		}
		if r.Origin.Channel != "" {
			if err := n.Tell(uctx, r); err != nil {
				s.logf("%s: outcome not posted in the conversation: %v", r.ID, err)
			}
		}
		cancel()
	}
	if s.o.Note != nil && r.Conversation != "" {
		if err := s.o.Note(r, NoteText(r)); err == nil {
			s.mu.Lock()
			if stored := s.file.Requests[r.ID]; stored != nil {
				stored.Noted = true
				_ = s.save()
			}
			s.mu.Unlock()
		}
	}
	if s.o.Hook != nil {
		s.o.Hook(wire.EventApprovalAnswered, r)
	}
}

// NoteText is a request's outcome as its conversation's next turn reads it: the runtime's note,
// never a person's words.
func NoteText(r *Request) string {
	head := fmt.Sprintf("[Approval %s — a runtime note, not a message from anyone: %s", r.ID, r.Card.Title)
	switch r.Status {
	case StatusDone:
		head += fmt.Sprintf(" was approved by %s and the runtime ran it", r.DecidedBy)
	case StatusFailed:
		head += " was approved but running it failed"
	case StatusDenied:
		head += fmt.Sprintf(" was denied by %s: it did not run", r.DecidedBy)
	case StatusExpired:
		head += " got no decision in time and was denied: it did not run"
	}
	if r.Trusted {
		head += "; the owner also trusted this conversation"
	}
	head += ".]"
	if r.Result != "" {
		head += "\nResult: " + r.Result
	}
	return head
}

// Notes are the outcomes of conversation's requests its transcript does not have yet, as rows for
// the next turn (taken: they are marked noted once done is called).
func (s *Service) Notes(conversation string) ([]wire.Message, func()) {
	if s == nil || conversation == "" {
		return nil, func() {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return nil, func() {}
	}
	var ids []string
	var rows []wire.Message
	list := s.sorted()
	for _, r := range list {
		if r.Conversation == conversation && r.Final() && !r.Noted && !s.busy[r.ID] {
			ids = append(ids, r.ID)
			rows = append(rows, wire.Message{Role: "user", Kind: wire.KindApproval, Content: NoteText(r)})
		}
	}
	return rows, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, id := range ids {
			if r := s.file.Requests[id]; r != nil {
				r.Noted = true
			}
		}
		_ = s.save()
	}
}

// Expire denies every pending request past its time (a tick, and at boot: one due while the
// runtime was down is denied when it starts), updating its cards and telling its conversation.
func (s *Service) Expire(ctx context.Context) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.load() != nil {
		s.mu.Unlock()
		return
	}
	expired := s.expireLocked(s.o.Now())
	for i, r := range expired {
		expired[i] = r.copy()
	}
	s.mu.Unlock()
	for _, r := range expired {
		s.audit(StatusExpired, r, "")
		s.finish(ctx, r)
	}
}

// expireLocked marks pending requests past due expired (s.mu held) and returns them.
func (s *Service) expireLocked(now time.Time) []*Request {
	var out []*Request
	for _, r := range s.file.Requests {
		if r.Pending() && !now.Before(r.Expires) {
			r.Status = StatusExpired
			out = append(out, r)
		}
	}
	if len(out) > 0 {
		if err := s.save(); err != nil {
			s.logf("expiries not kept: %v", err)
		}
	}
	return out
}

// prune drops decided requests older than keepDecided, and the oldest past maxEntries (s.mu held);
// a decided request's payload goes once it is noted.
func (s *Service) prune(now time.Time) {
	for id, r := range s.file.Requests {
		if r.Final() && now.Sub(r.Created) > keepDecided {
			delete(s.file.Requests, id)
			continue
		}
		if r.Final() && (r.Noted || r.Conversation == "") {
			r.Action.Args = nil
			if r.Action.Delivery != nil {
				r.Action.Delivery.Text = ""
			}
			r.Card.Payload = ""
		}
	}
	if len(s.file.Requests) >= maxEntries {
		list := s.sorted()
		for _, r := range list {
			if len(s.file.Requests) < maxEntries {
				break
			}
			if r.Final() {
				delete(s.file.Requests, r.ID)
			}
		}
	}
}

func (s *Service) sorted() []*Request {
	list := make([]*Request, 0, len(s.file.Requests))
	for _, r := range s.file.Requests {
		list = append(list, r)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.Before(list[j].Created) })
	return list
}

// Get is a request by id (a copy).
func (s *Service) Get(id string) (*Request, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return nil, false
	}
	r, ok := s.file.Requests[id]
	if !ok {
		return nil, false
	}
	return r.copy(), true
}

// List is every request kept, oldest first (copies).
func (s *Service) List() []*Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.load() != nil {
		return nil
	}
	out := []*Request{}
	for _, r := range s.sorted() {
		out = append(out, r.copy())
	}
	return out
}

// Boot loads the store and denies what fell due while the runtime was down.
func (s *Service) Boot(ctx context.Context) error {
	s.mu.Lock()
	err := s.load()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.Expire(ctx)
	return nil
}

func (r *Request) copy() *Request {
	c := *r
	c.Cards = append([]Posted{}, r.Cards...)
	c.Taint = append([]wire.Taint{}, r.Taint...)
	if r.Action.Delivery != nil {
		d := *r.Action.Delivery
		d.Targets = append([]string{}, d.Targets...)
		c.Action.Delivery = &d
	}
	return &c
}

// newID is ap_ + the time in Unix milliseconds (hex) + 6 random hex digits.
func newID(now time.Time) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("ap_%x%s", now.UnixMilli(), hex.EncodeToString(b))
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}
