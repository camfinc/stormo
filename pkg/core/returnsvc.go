package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/whatsapp"
)

// The core's return paths (docs/core.md §5, "Returns"): a person's WhatsApp message is an origin;
// the turn the core runs for it is an execution; a request the agent sends over the bus during it
// is a return, whose events (an answer, progress, an ack, the bus's notices, a deadline) bring the
// agent back into that chat through a delivery: a channel turn whose reply the core posts there,
// part by part through a write-ahead journal. The state machine is returns.go; this is its
// database, its turns and its posting.

// ReturnPoster is the agent's channel as the returns post to it (the WhatsApp connector).
type ReturnPoster interface {
	AllowedReply(o whatsapp.Origin) error
	PostOrigin(ctx context.Context, o whatsapp.Origin, parts []string, from int, journal func(part int, state, id string) error) error
}

// ReturnsOptions configure the return paths.
type ReturnsOptions struct {
	DB  *DB
	Bus *Bus
	// Settings is an agent's bus.returns (nil: off for it); Mode the instance's core.returns.
	Settings func(agent string) *manifest.Returns
	Mode     func() string
	// Channel is the agent's channel-turn client; Poster its channel.
	Channel func(agent string) (engine.ChannelTurns, error)
	Poster  func(agent string) (ReturnPoster, error)
	Now     func() time.Time
}

// Returns is the core's return paths; safe for concurrent use.
type Returns struct {
	o    ReturnsOptions
	kick chan struct{}
	mu   sync.Mutex // one round at a time
	caps sync.Map   // agent → capsEntry
}

type capsEntry struct {
	caps engine.ChannelCaps
	at   time.Time
}

// capsTTL is how long a negotiation is trusted before it is asked again.
const capsTTL = time.Minute

// NewReturns builds the return paths; call Run for their worker.
func NewReturns(o ReturnsOptions) *Returns {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Mode == nil {
		o.Mode = func() string { return "on" }
	}
	r := &Returns{o: o, kick: make(chan struct{}, 1)}
	if o.Bus != nil {
		o.Bus.returns = r
	}
	return r
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Kick asks the worker for a round now.
func (r *Returns) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// negotiated is what agent's engine offers now (asked again after capsTTL; an engine that cannot
// say offers nothing). Never called inside a transaction: it may ask the engine (cached reads it).
func (r *Returns) negotiated(agent string) engine.ChannelCaps {
	if v, ok := r.caps.Load(agent); ok {
		if e := v.(capsEntry); r.o.Now().Sub(e.at) < capsTTL {
			return e.caps
		}
	}
	return r.refresh(agent)
}

// cached is the last negotiation (stale up to two TTLs, else nothing): for the bus's transactions,
// which must not wait on an engine.
func (r *Returns) cached(agent string) engine.ChannelCaps {
	if v, ok := r.caps.Load(agent); ok {
		if e := v.(capsEntry); r.o.Now().Sub(e.at) < 2*capsTTL {
			return e.caps
		}
	}
	return engine.ChannelCaps{}
}

// refresh asks agent's engine what it offers.
func (r *Returns) refresh(agent string) engine.ChannelCaps {
	var caps engine.ChannelCaps
	if r.o.Channel != nil {
		if ct, err := r.o.Channel(agent); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			caps, _ = ct.Capabilities(ctx)
			cancel()
		}
	}
	r.caps.Store(agent, capsEntry{caps: caps, at: r.o.Now()})
	return caps
}

// on is whether agent's channel turns run through the return paths: it opted in, the instance's
// switch is not stop, and its engine runs channel turns.
func (r *Returns) on(agent string) bool {
	return r.o.Settings != nil && r.o.Settings(agent) != nil && r.o.Mode() != "stop" && r.negotiated(agent).Turns
}

// opens is whether a request may open a return now (inside the bus's transaction: the cached
// negotiation): the agent opted in, not draining, and its engine runs channel turns and enforces
// them (its channel's conversations run only so, so every request is attributed).
func (r *Returns) opens(agent string) bool {
	c := r.cached(agent)
	return r.o.Settings != nil && r.o.Settings(agent) != nil && r.o.Mode() == "on" && c.Turns && c.Enforcing
}

// RegisterOrigin records an accepted WhatsApp message (idempotent on its id): its origin id, ""
// when the agent's channel turns do not run through the return paths.
func (r *Returns) RegisterOrigin(agent string, e whatsapp.OriginEvent) (string, error) {
	if !r.on(agent) {
		return "", nil
	}
	id := newID()
	_, err := r.o.DB.Exec(`INSERT OR IGNORE INTO origins(id, agent, platform, chat, requester, requester_verified, message_id, quote, session, created)
		VALUES(?,?,?,?,?,?,?,?,?,?)`, id, agent, "whatsapp", e.Chat, e.Number+"\x00"+e.Name+"\x00"+e.Participant, e.Verified, e.MessageID, hex.EncodeToString(e.Quote), e.Session, r.o.Now().UnixMilli())
	if err != nil {
		return "", err
	}
	var got string
	err = r.o.DB.QueryRow(`SELECT id FROM origins WHERE agent = ? AND platform = 'whatsapp' AND chat = ? AND message_id = ?`, agent, e.Chat, e.MessageID).Scan(&got)
	return got, err
}

// originOf is a recorded origin as the connector posts to it, and the requester's name.
func (r *Returns) originOf(id string) (whatsapp.Origin, string, error) {
	var chat, requester, msgID, quote string
	var group bool
	err := r.o.DB.QueryRow(`SELECT chat, requester, message_id, quote, chat LIKE '%@g.us' FROM origins WHERE id = ?`, id).Scan(&chat, &requester, &msgID, &quote, &group)
	if err != nil {
		return whatsapp.Origin{}, "", err
	}
	parts := strings.SplitN(requester, "\x00", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	q, _ := hex.DecodeString(quote)
	return whatsapp.Origin{Chat: chat, MessageID: msgID, Participant: parts[2], Number: parts[0], Group: group, Quote: q}, parts[1], nil
}

// ErrNoReturns is a turn that does not run through the return paths (the caller's usual path).
var ErrNoReturns = errors.New("returns: not for this turn")

// InboundTurn runs a person's message as a channel turn (its origin, so what the agent sends while
// answering it can come back): its answer; ErrNoReturns when the turn is not one (no origin);
// whatsapp.ErrPending when ctx ended first (the reply is posted when the turn ends).
func (r *Returns) InboundTurn(ctx context.Context, agent, session string, t whatsapp.Turn) (whatsapp.Answer, error) {
	if t.Origin == "" || !r.on(agent) {
		return whatsapp.Answer{}, ErrNoReturns
	}
	ct, err := r.o.Channel(agent)
	if err != nil {
		return whatsapp.Answer{}, ErrNoReturns
	}
	exec := newID()
	if _, err := r.o.DB.Exec(`INSERT INTO executions(id, agent, kind, origin, session, state, created) VALUES(?,?,?,?,?,?,?)`,
		exec, agent, "inbound", t.Origin, session, "running", r.o.Now().UnixMilli()); err != nil {
		return whatsapp.Answer{}, err
	}
	in := engine.Input{Text: t.Text, AudioFormat: "opus"}
	if a := t.Audio; a != nil {
		in.Files = []engine.File{{Name: "voice-note.ogg", MIME: a.MIME, Data: a.Data, Marked: a.Marked}}
	}
	turn := engine.ChannelTurn{Execution: exec, Kind: "inbound", Session: session, In: in}
	st, err := r.start(ctx, ct, turn)
	if err != nil {
		r.endExecution(exec, "failed", 0, err.Error())
		return whatsapp.Answer{}, err
	}
	st, err = r.await(ctx, ct, exec, st)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		go r.finishInbound(agent, t.Origin, ct, exec)
		return whatsapp.Answer{}, whatsapp.ErrPending
	}
	if err != nil {
		return whatsapp.Answer{}, err
	}
	r.endExecution(exec, st.State, st.Acted, st.Error)
	_ = ct.Take(context.Background(), exec)
	if st.State != "ok" {
		return whatsapp.Answer{}, fmt.Errorf("the turn %s: %s", st.State, st.Error)
	}
	ans := whatsapp.Answer{Text: st.Text}
	if a := st.Audio; a != nil && strings.HasPrefix(a.MIME, "audio/ogg") {
		ans.Voice, ans.WithText = a.Data, a.WithText
	}
	return ans, nil
}

// start admits a channel turn, waiting while its conversation runs another.
func (r *Returns) start(ctx context.Context, ct engine.ChannelTurns, t engine.ChannelTurn) (engine.ChannelTurnStatus, error) {
	for {
		st, err := ct.Start(ctx, t)
		if !errors.Is(err, engine.ErrChannelBusy) {
			return st, err
		}
		select {
		case <-ctx.Done():
			return engine.ChannelTurnStatus{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// await polls a channel turn until it ends or ctx does.
func (r *Returns) await(ctx context.Context, ct engine.ChannelTurns, exec string, st engine.ChannelTurnStatus) (engine.ChannelTurnStatus, error) {
	for !st.Terminal() {
		var err error
		if st, err = ct.Status(ctx, exec, 30*time.Second); err != nil {
			if ctx.Err() != nil {
				return st, ctx.Err()
			}
			select {
			case <-ctx.Done():
				return st, ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	}
	return st, nil
}

// finishInbound waits for a person's turn the connector stopped waiting for, and posts its reply.
func (r *Returns) finishInbound(agent, origin string, ct engine.ChannelTurns, exec string) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	st, err := r.await(ctx, ct, exec, engine.ChannelTurnStatus{})
	if err != nil {
		return
	}
	r.endExecution(exec, st.State, st.Acted, st.Error)
	text := withoutSilence(st.Text)
	if st.State != "ok" || strings.TrimSpace(text) == "" {
		_ = ct.Take(context.Background(), exec)
		return
	}
	o, _, err := r.originOf(origin)
	p, perr := r.o.Poster(agent)
	if err == nil && perr == nil && p.AllowedReply(o) == nil {
		_ = p.PostOrigin(ctx, o, whatsapp.Parts(text), 0, func(int, string, string) error { return nil })
	}
	_ = ct.Take(context.Background(), exec)
}

// withoutSilence is a reply without a [SILENT] answer.
func withoutSilence(s string) string {
	if strings.TrimSpace(s) == "[SILENT]" {
		return ""
	}
	return s
}

func (r *Returns) endExecution(exec, state string, acted int64, errText string) {
	_, _ = r.o.DB.Exec(`UPDATE executions SET state = ?, acted = ?, error = ?, ended = ? WHERE id = ? AND ended IS NULL`,
		state, acted, clip(errText, 300), r.o.Now().UnixMilli(), exec)
}

// ---- the bus's side (all in its transactions) ------------------------------------------------

// sendOrigin is a channel turn's request as the bus is about to store it: the origin and parent
// return its execution gives, or why it is refused. A send from no execution has none.
func (r *Returns) sendOrigin(tx *sql.Tx, sender, exec string, thread int64) (origin string, parent int64, err error) {
	if exec == "" {
		return "", 0, nil
	}
	var agent, kind string
	var ret sql.NullInt64
	if err := tx.QueryRow(`SELECT agent, kind, origin, ret FROM executions WHERE id = ?`, exec).Scan(&agent, &kind, &origin, &ret); err != nil {
		return "", 0, nil // an execution the core does not know (another core's, a test's): no return
	}
	if agent != sender {
		return "", 0, toolErr("this turn's execution is not yours")
	}
	if thread != 0 {
		// one person's conversation per bus thread: an update must never go to someone else's chat
		var other int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM returns WHERE thread = ? AND origin != ? AND closed_at IS NULL`, thread, origin).Scan(&other); err != nil {
			return "", 0, err
		}
		if other > 0 {
			return "", 0, toolErr("this thread already carries a request for another conversation: start a new one (leave out thread)")
		}
	}
	return origin, ret.Int64, nil
}

// open makes a return of request msg (sent by sender to recipient in thread) when its origin is
// given and the agent's returns are open.
func (r *Returns) open(tx *sql.Tx, sender, origin string, parent, msg, thread int64, recipient string) {
	if origin == "" || !r.opens(sender) {
		return
	}
	set := r.o.Settings(sender)
	sched, loc := set.Schedule()
	now := r.o.Now()
	depth := 0
	if parent != 0 {
		v, err := r.view(tx, parent)
		if err != nil {
			return
		}
		step, ok := OnChild(v, now)
		if !ok {
			return // the chain is too deep, or its root closed: no return
		}
		if err := r.save(tx, parent, step.Next); err != nil {
			return
		}
		depth = v.Depth + 1
	}
	_, err := tx.Exec(`INSERT INTO returns(request, thread, agent, recipient, origin, parent, depth, state, escalate_at, next_due, last_activity, created)
		VALUES(?,?,?,?,?,NULLIF(?,0),?,?,?,?,?,?)`, msg, thread, sender, recipient, origin, parent, depth, ReturnOpen,
		sched.AddWorking(now, set.EscalateAfter, loc).UnixMilli(), sched.AddWorking(now, set.UpdateAfter, loc).UnixMilli(), now.UnixMilli(), now.UnixMilli())
	if err != nil {
		log.Printf("returns: open: %v", err)
	}
}

// root is the return events of ret are delivered through: itself, or its topmost parent.
func (r *Returns) root(tx *sql.Tx, ret int64) (int64, bool) {
	child := false
	for i := 0; i < ReturnChildDepth+1; i++ {
		var parent sql.NullInt64
		if err := tx.QueryRow(`SELECT parent FROM returns WHERE id = ?`, ret).Scan(&parent); err != nil || !parent.Valid {
			return ret, child
		}
		ret, child = parent.Int64, true
	}
	return ret, child
}

// event records an event of return ret (its root's, child-marked, when ret is a child); its
// delivery to the return's agent (deliveryOf, 0 for none) is then the return's to wake.
func (r *Returns) event(tx *sql.Tx, ret int64, kind string, message int64, deliveryAgent string) {
	root, child := r.root(tx, ret)
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM return_events WHERE ret = ?`, root).Scan(&seq); err != nil {
		return
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO return_events(ret, seq, kind, message, child, at) VALUES(?,?,?,?,?,?)`,
		root, seq, kind, message, child, r.o.Now().UnixMilli())
	if err != nil {
		log.Printf("returns: event: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // the same event again
	}
	if message != 0 && deliveryAgent != "" {
		_, _ = tx.Exec(`UPDATE deliveries SET owner = 'return' WHERE message = ? AND agent = ?`, message, deliveryAgent)
	}
	r.Kick()
}

// onSend records the events a stored message brings: an answer to the requests it names, progress
// on a request in its thread (when one person's requests are the only ones it could be about).
func (r *Returns) onSend(tx *sql.Tx, msg int64, sender string, recipients []BusAgent, thread int64, answers []int64) {
	for _, a := range answers {
		var ret int64
		var agent string
		if tx.QueryRow(`SELECT id, agent FROM returns WHERE request = ? AND recipient = ?`, a, sender).Scan(&ret, &agent) == nil {
			r.event(tx, ret, EventAnswered, msg, agent)
		}
	}
	if len(answers) > 0 || thread == 0 || len(recipients) != 1 {
		return
	}
	rows, err := tx.Query(`SELECT id, origin FROM returns WHERE thread = ? AND recipient = ? AND agent = ? AND closed_at IS NULL ORDER BY id DESC`,
		thread, sender, recipients[0].ID)
	if err != nil {
		return
	}
	var ids []int64
	origins := map[string]bool{}
	for rows.Next() {
		var id int64
		var o string
		if rows.Scan(&id, &o) == nil {
			ids = append(ids, id)
			origins[o] = true
		}
	}
	rows.Close()
	switch {
	case len(ids) == 0:
	case len(origins) > 1:
		// it could be about more than one person's request: it stays the bus's, never routed
		raiseFindingTx(tx, sender, "return_ambiguous", "medium", fmt.Sprintf("message %d", msg), r.o.Now().UnixMilli())
	default:
		r.event(tx, ids[0], EventProgress, msg, recipients[0].ID)
	}
}

// onNotice records the bus's own notice about request id to its sender as the return's event.
func (r *Returns) onNotice(tx *sql.Tx, request, notice int64, sender string, told int) {
	var ret int64
	if tx.QueryRow(`SELECT id FROM returns WHERE request = ? AND agent = ?`, request, sender).Scan(&ret) != nil {
		return
	}
	kind := EventStatus
	if told == toldDone {
		kind = EventFailure
	}
	r.event(tx, ret, kind, notice, sender)
}

// onAck records the recipient acking the request without an answer.
func (r *Returns) onAck(request int64, acker string) {
	tx, err := r.o.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	var ret int64
	var answered int
	if tx.QueryRow(`SELECT id FROM returns WHERE request = ? AND recipient = ?`, request, acker).Scan(&ret) != nil {
		return
	}
	// an ack an answer made (msg_send answers) is the answer's event already
	_ = tx.QueryRow(`SELECT COUNT(*) FROM answers a JOIN messages m ON m.id = a.message WHERE a.answered = ? AND m.sender = ?`, request, acker).Scan(&answered)
	if answered > 0 {
		return
	}
	r.event(tx, ret, EventAcked, 0, "")
	_ = tx.Commit()
}

// raiseFindingTx is DB.RaiseFinding inside a transaction (core.db has one connection).
func raiseFindingTx(tx *sql.Tx, agent, rule, severity, evidence string, at int64) {
	_, _ = tx.Exec(`INSERT INTO findings(agent, rule, severity, evidence, first_seen, last_seen) VALUES(?,?,?,?,?,?)
		ON CONFLICT(agent, rule, evidence) DO UPDATE SET last_seen = excluded.last_seen, count = count + 1, severity = excluded.severity,
		status = CASE WHEN status = 'resolved' THEN 'open' ELSE status END`, agent, rule, severity, evidence, at, at)
}

// sideDoor refuses a return turn's whatsapp_send to its own origin's chat: an update to the person
// goes through the journal (the turn's reply), never around it. The chat is compared as the person
// it is (a group's id; a direct chat's number, whichever address the origin carried).
func (r *Returns) sideDoor(agent, exec, chat string) error {
	if exec == "" {
		return nil
	}
	var kind, origin string
	if r.o.DB.QueryRow(`SELECT kind, origin FROM executions WHERE id = ? AND agent = ?`, exec, agent).Scan(&kind, &origin) != nil || kind != "return" {
		return nil
	}
	o, _, err := r.originOf(origin)
	if err != nil {
		return nil
	}
	target := strings.TrimSpace(chat)
	same := target == o.Chat
	if !o.Group && o.Number != "" {
		same = same || strings.TrimPrefix(target, "+") == o.Number
	}
	if same {
		return toolErr("this turn's update to the person goes as your reply here, not through whatsapp_send")
	}
	return nil
}
