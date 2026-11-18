package core

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/whatsapp"
	"go.mau.fi/whatsmeow/types"
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
	o        ReturnsOptions
	kick     chan struct{}
	mu       sync.Mutex // one round at a time
	caps     sync.Map   // agent → capsEntry
	scrubbed time.Time  // the last retention pass (under mu)
}

// capsEntry is an agent's negotiation: the last answer its engine gave (ok: when; zero, never) and
// when it was last asked. An engine that cannot be reached keeps its last answer: a restart must not
// read as the engine no longer running channel turns.
type capsEntry struct {
	caps   engine.ChannelCaps
	asked  time.Time
	answer time.Time
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

// newToken is an execution's secret.
func newToken() string {
	b := make([]byte, 24)
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

// negotiated is what agent's engine offers (asked again after capsTTL; its last answer while it
// cannot be reached). Never called inside a transaction: it may ask the engine.
func (r *Returns) negotiated(agent string) engine.ChannelCaps {
	if v, ok := r.caps.Load(agent); ok {
		if e := v.(capsEntry); r.o.Now().Sub(e.asked) < capsTTL {
			return e.caps
		}
	}
	return r.refresh(agent)
}

// cached is the last answer if it is recent (two TTLs), else nothing: for the bus's transactions,
// which must not wait on an engine, and which open a return only on a fresh negotiation.
func (r *Returns) cached(agent string) engine.ChannelCaps {
	if v, ok := r.caps.Load(agent); ok {
		if e := v.(capsEntry); !e.answer.IsZero() && r.o.Now().Sub(e.answer) < 2*capsTTL {
			return e.caps
		}
	}
	return engine.ChannelCaps{}
}

// known is the engine's last answer, however old, and whether it ever gave one.
func (r *Returns) known(agent string) (engine.ChannelCaps, bool) {
	if v, ok := r.caps.Load(agent); ok {
		if e := v.(capsEntry); !e.answer.IsZero() {
			return e.caps, true
		}
	}
	return engine.ChannelCaps{}, false
}

// refresh asks agent's engine what it offers; an engine that does not answer keeps its last answer.
func (r *Returns) refresh(agent string) engine.ChannelCaps {
	now := r.o.Now()
	var prev capsEntry
	if v, ok := r.caps.Load(agent); ok {
		prev = v.(capsEntry)
	}
	next := capsEntry{caps: prev.caps, asked: now, answer: prev.answer}
	if r.o.Channel != nil {
		if ct, err := r.o.Channel(agent); err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if caps, err := ct.Capabilities(ctx); err == nil {
				next.caps, next.answer = caps, now
			}
			cancel()
		}
	}
	r.caps.Store(agent, next)
	return next.caps
}

// optedIn is whether agent has bus.returns on.
func (r *Returns) optedIn(agent string) bool {
	return r.o.Settings != nil && r.o.Settings(agent) != nil
}

// channel is whether agent's WhatsApp turns run as channel turns: its engine runs them, and either
// enforces them (its WhatsApp conversations run no other way) or the agent opted in to the returns.
// An agent that did neither keeps the engine's usual chat, whatever its image offers.
func (r *Returns) channel(agent string) bool {
	c := r.negotiated(agent)
	return c.Turns && (c.Enforcing || r.optedIn(agent))
}

// opens is whether a request may open a return now (inside the bus's transaction: the cached
// negotiation): the agent opted in, the instance's switch is on, and its engine runs channel turns
// and enforces them (so every request from its WhatsApp conversations is attributed).
func (r *Returns) opens(agent string) bool {
	c := r.cached(agent)
	return r.optedIn(agent) && r.o.Mode() == "on" && c.Turns && c.Enforcing
}

// draining is why agent's returns give their work back to the bus ("" when they do not): the agent
// opted out, the instance drains, or its engine answered it no longer enforces channel turns.
func (r *Returns) draining(agent string) string {
	switch {
	case !r.optedIn(agent):
		return "the agent's returns are off"
	case r.o.Mode() == "drain":
		return "the return paths are draining"
	}
	if c, ok := r.known(agent); ok && !(c.Turns && c.Enforcing) {
		return "the agent's engine no longer enforces channel turns"
	}
	return ""
}

// RegisterOrigin records an accepted WhatsApp message (idempotent on its id): its origin id, ""
// when the agent's WhatsApp turns do not run as channel turns.
func (r *Returns) RegisterOrigin(agent string, e whatsapp.OriginEvent) (string, error) {
	if !r.channel(agent) {
		return "", nil
	}
	id := newID()
	_, err := r.o.DB.Exec(`INSERT OR IGNORE INTO origins(id, agent, platform, account, chat, requester, requester_verified, message_id, quote, session, created)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, agent, "whatsapp", e.Account, e.Chat, e.Number+"\x00"+e.Name+"\x00"+e.Participant, e.Verified, e.MessageID,
		hex.EncodeToString(e.Quote), e.Session, r.o.Now().UnixMilli())
	if err != nil {
		return "", err
	}
	var got string
	err = r.o.DB.QueryRow(`SELECT id FROM origins WHERE agent = ? AND platform = 'whatsapp' AND chat = ? AND message_id = ?`, agent, e.Chat, e.MessageID).Scan(&got)
	return got, err
}

// originOf is a recorded origin as the connector posts to it, and the requester's name.
func (r *Returns) originOf(id string) (whatsapp.Origin, string, error) {
	var chat, account, requester, msgID, quote string
	var group bool
	err := r.o.DB.QueryRow(`SELECT chat, account, requester, message_id, quote, chat LIKE '%@g.us' FROM origins WHERE id = ?`, id).
		Scan(&chat, &account, &requester, &msgID, &quote, &group)
	if err != nil {
		return whatsapp.Origin{}, "", err
	}
	parts := strings.SplitN(requester, "\x00", 3)
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	q, _ := hex.DecodeString(quote)
	return whatsapp.Origin{Chat: chat, MessageID: msgID, Participant: parts[2], Number: parts[0], Account: account, Group: group, Quote: q}, parts[1], nil
}

// ErrNoReturns is a turn that does not run as a channel turn (the caller's usual path).
var ErrNoReturns = errors.New("returns: not for this turn")

// InboundTurn runs a person's message as a channel turn (its origin, so what the agent sends while
// answering it can come back): its answer; ErrNoReturns when the turn is not one (no origin);
// whatsapp.ErrPending when ctx ended first (the worker posts its reply when it ends).
func (r *Returns) InboundTurn(ctx context.Context, agent, session string, t whatsapp.Turn) (whatsapp.Answer, error) {
	if t.Origin == "" || !r.channel(agent) {
		return whatsapp.Answer{}, ErrNoReturns
	}
	ct, err := r.o.Channel(agent)
	if err != nil {
		return whatsapp.Answer{}, ErrNoReturns
	}
	exec, token := newID(), newToken()
	if _, err := r.o.DB.Exec(`INSERT INTO executions(id, agent, kind, origin, token, session, state, created) VALUES(?,?,?,?,?,?,?,?)`,
		exec, agent, "inbound", t.Origin, token, session, "running", r.o.Now().UnixMilli()); err != nil {
		return whatsapp.Answer{}, err
	}
	in := engine.Input{Text: t.Text, AudioFormat: "opus"}
	if a := t.Audio; a != nil {
		in.Files = []engine.File{{Name: "voice-note.ogg", MIME: a.MIME, Data: a.Data, Marked: a.Marked}}
	}
	turn := engine.ChannelTurn{Execution: exec, Token: token, Kind: "inbound", Session: session, In: in}
	st, err := r.admit(ctx, ct, turn)
	if err == nil {
		st, err = r.await(ctx, ct, exec, st)
	}
	switch {
	case ctx.Err() != nil:
		// the connector stopped waiting: the worker asks about it (with the same execution) and posts
		// its reply through the journal when it ends
		_, _ = r.o.DB.Exec(`UPDATE executions SET state = 'detached' WHERE id = ? AND ended IS NULL`, exec)
		r.Kick()
		return whatsapp.Answer{}, whatsapp.ErrPending
	case err != nil:
		r.endExecution(exec, "failed", 0, err.Error())
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

// admit starts a channel turn: while its conversation runs another it waits; an answer it did not
// get (a lost response, a runtime restarting) is asked again with the same execution and request,
// which the runtime answers with the turn it admitted, if it did. Only ctx ending, or the runtime
// refusing it, stops it.
func (r *Returns) admit(ctx context.Context, ct engine.ChannelTurns, t engine.ChannelTurn) (engine.ChannelTurnStatus, error) {
	for {
		st, err := ct.Start(ctx, t)
		if err == nil || errors.Is(err, engine.ErrChannelRefused) {
			return st, err
		}
		gap := 3 * time.Second
		if errors.Is(err, engine.ErrChannelBusy) {
			gap = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return engine.ChannelTurnStatus{}, ctx.Err()
		case <-time.After(gap):
		}
	}
}

// await polls a channel turn until it ends, the runtime does not know it, or ctx ends.
func (r *Returns) await(ctx context.Context, ct engine.ChannelTurns, exec string, st engine.ChannelTurnStatus) (engine.ChannelTurnStatus, error) {
	for !st.Terminal() {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		next, err := ct.Status(ctx, exec, 30*time.Second)
		switch {
		case err == nil:
			st = next
			continue
		case errors.Is(err, engine.ErrChannelUnknown):
			return st, err
		case ctx.Err() != nil:
			return st, ctx.Err()
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return st, nil
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

// Verified is exec when the call carries its token and it is agent's, else "": what a call claims
// of a turn is believed only with the secret only that turn's runtime holds.
func (r *Returns) Verified(agent, exec, token string) string {
	if exec == "" || token == "" {
		return ""
	}
	var tok string
	if r.o.DB.QueryRow(`SELECT token FROM executions WHERE id = ? AND agent = ?`, exec, agent).Scan(&tok) != nil || tok == "" ||
		subtle.ConstantTimeCompare([]byte(tok), []byte(token)) != 1 {
		return ""
	}
	return exec
}

// ---- the bus's side (all in its transactions) ------------------------------------------------

// sendFrom is what a send's execution gives the request: the person's conversation it is for
// (origin, "" none), and when it comes from an update's turn, that return (parent) with its depth
// and escalation time. note tells the agent the request is not followed for the person, and why.
type sendFrom struct {
	origin     string
	parent     int64
	depth      int
	escalateAt time.Time
	note       string
}

// sendOrigin is a channel turn's request as the bus is about to store it, or why it is refused: a
// call naming an execution the core did not start, another agent's, or without its token; a thread
// that ever carried another person's request. A send from no execution has none.
func (r *Returns) sendOrigin(tx *sql.Tx, sender, exec, token string, thread int64) (sendFrom, error) {
	var f sendFrom
	if exec == "" {
		return f, nil
	}
	var agent, tok string
	var ret sql.NullInt64
	err := tx.QueryRow(`SELECT agent, origin, token, ret FROM executions WHERE id = ?`, exec).Scan(&agent, &f.origin, &tok, &ret)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return f, toolErr("this call names a turn the core did not start")
	case err != nil:
		return f, err
	case agent != sender:
		return f, toolErr("this turn's execution is not yours")
	case tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(token)) != 1:
		return f, toolErr("this call does not carry its turn's token")
	}
	if thread != 0 {
		// one person's conversation per bus thread, for its whole life: an update must never reach
		// someone else's chat
		var other int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM returns WHERE thread = ? AND origin != ?`, thread, f.origin).Scan(&other); err != nil {
			return f, err
		}
		if other > 0 {
			return f, toolErr("this thread carries a request for another person's conversation: start a new one (leave out thread)")
		}
	}
	if ret.Valid {
		v, err := r.view(tx, ret.Int64)
		if err != nil {
			return f, err
		}
		if why := MayDelegate(v, r.o.Now()); why != "" {
			f.origin, f.note = "", "not followed for the person ("+why+"): their outcome will not come back to their chat on its own"
			return f, nil
		}
		f.parent, f.depth, f.escalateAt = ret.Int64, v.Depth+1, v.EscalateAt
	}
	return f, nil
}

// open makes a return of request msg (sent by sender to recipient in thread) when it is for a
// person's conversation and the agent's returns are open. A request from an update's turn keeps
// that return's escalation time.
func (r *Returns) open(tx *sql.Tx, sender string, f sendFrom, msg, thread int64, recipient string) error {
	if f.origin == "" || !r.opens(sender) {
		return nil
	}
	set := r.o.Settings(sender)
	sched, loc := set.Schedule()
	now := r.o.Now()
	escalate := sched.AddWorking(now, set.EscalateAfter, loc)
	if f.parent != 0 && !f.escalateAt.IsZero() {
		escalate = f.escalateAt
	}
	_, err := tx.Exec(`INSERT INTO returns(request, thread, agent, recipient, origin, parent, depth, state, escalate_at, next_due, last_activity, created)
		VALUES(?,?,?,?,?,NULLIF(?,0),?,?,?,?,?,?)`, msg, thread, sender, recipient, f.origin, f.parent, f.depth, ReturnOpen,
		unms(escalate), unms(sched.AddWorking(now, set.UpdateAfter, loc)), now.UnixMilli(), now.UnixMilli())
	return err
}

// event records an event of return ret when it can still reach the person (Eligible), and takes its
// message's delivery to the return's agent (deliveryAgent) from the bus: the return wakes the agent
// for it. One that cannot stays the bus's, unrecorded.
func (r *Returns) event(tx *sql.Tx, ret int64, kind string, message int64, deliveryAgent string) error {
	v, err := r.view(tx, ret)
	if err != nil {
		return err
	}
	if !Eligible(v, kind) {
		return nil
	}
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM return_events WHERE ret = ?`, ret).Scan(&seq); err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO return_events(ret, seq, kind, message, at) VALUES(?,?,?,?,?)`, ret, seq, kind, message, r.o.Now().UnixMilli())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // the same event again
	}
	if message != 0 && deliveryAgent != "" {
		if _, err := tx.Exec(`UPDATE deliveries SET owner = 'return' WHERE message = ? AND agent = ?`, message, deliveryAgent); err != nil {
			return err
		}
	}
	r.Kick()
	return nil
}

// onSend records the events a stored message brings: an answer to the requests it names (refused
// when they were made for different people), progress on a request in its thread (when one
// person's requests are the only ones it could be about).
func (r *Returns) onSend(tx *sql.Tx, msg int64, sender string, recipients []BusAgent, thread int64, answers []int64) error {
	type hit struct {
		ret   int64
		agent string
	}
	var hits []hit
	people := map[string]bool{}
	for _, a := range answers {
		var h hit
		var origin string
		err := tx.QueryRow(`SELECT id, agent, origin FROM returns WHERE request = ? AND recipient = ?`, a, sender).Scan(&h.ret, &h.agent, &origin)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		hits, people[origin] = append(hits, h), true
	}
	if len(people) > 1 {
		return toolErr("this message answers requests made for different people: answer each in a message of its own")
	}
	for _, h := range hits {
		if err := r.event(tx, h.ret, EventAnswered, msg, h.agent); err != nil {
			return err
		}
	}
	if len(answers) > 0 || thread == 0 || len(recipients) != 1 {
		return nil
	}
	rows, err := tx.Query(`SELECT id, origin FROM returns WHERE thread = ? AND recipient = ? AND agent = ? AND closed_at IS NULL ORDER BY id DESC`,
		thread, sender, recipients[0].ID)
	if err != nil {
		return err
	}
	var ids []int64
	origins := map[string]bool{}
	for rows.Next() {
		var id int64
		var o string
		if err := rows.Scan(&id, &o); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		origins[o] = true
	}
	rows.Close()
	switch {
	case len(ids) == 0:
	case len(origins) > 1:
		// it could be about more than one person's request: it stays the bus's, never routed
		raiseFindingTx(tx, sender, "return_ambiguous", "medium", fmt.Sprintf("message %d", msg), r.o.Now().UnixMilli())
	default:
		return r.event(tx, ids[0], EventProgress, msg, recipients[0].ID)
	}
	return nil
}

// onNotice records the bus's own notice about request id to its sender as the return's event.
func (r *Returns) onNotice(tx *sql.Tx, request, notice int64, sender string, told int) error {
	var ret int64
	err := tx.QueryRow(`SELECT id FROM returns WHERE request = ? AND agent = ?`, request, sender).Scan(&ret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	kind := EventStatus
	if told == toldDone {
		kind = EventFailure
	}
	return r.event(tx, ret, kind, notice, sender)
}

// onAck records the recipient acking the request without an answer (in the ack's transaction).
func (r *Returns) onAck(tx *sql.Tx, request int64, acker string) error {
	var ret int64
	err := tx.QueryRow(`SELECT id FROM returns WHERE request = ? AND recipient = ?`, request, acker).Scan(&ret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// an ack an answer made (msg_send answers) is the answer's event already
	var answered int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM answers a JOIN messages m ON m.id = a.message WHERE a.answered = ? AND m.sender = ?`, request, acker).Scan(&answered); err != nil {
		return err
	}
	if answered > 0 {
		return nil
	}
	return r.event(tx, ret, EventAcked, 0, "")
}

// raiseFindingTx is DB.RaiseFinding inside a transaction (core.db has one connection).
func raiseFindingTx(tx *sql.Tx, agent, rule, severity, evidence string, at int64) {
	_, _ = tx.Exec(`INSERT INTO findings(agent, rule, severity, evidence, first_seen, last_seen) VALUES(?,?,?,?,?,?)
		ON CONFLICT(agent, rule, evidence) DO UPDATE SET last_seen = excluded.last_seen, count = count + 1, severity = excluded.severity,
		status = CASE WHEN status = 'resolved' THEN 'open' ELSE status END`, agent, rule, severity, evidence, at, at)
}

// sideDoor refuses agent's whatsapp_send to a chat one of its update turns is running for, whatever
// the call claims of itself: an update to the person goes through the journal (the turn's reply),
// never around it. A chat is the person it is: a group's id, or a direct chat's number however the
// origin addressed it (a group's requester written to directly is another chat).
func (r *Returns) sideDoor(agent string, jid types.JID) error {
	rows, err := r.o.DB.Query(`SELECT o.chat, o.requester FROM executions x JOIN origins o ON o.id = x.origin
		WHERE x.agent = ? AND x.kind = 'return' AND x.state = 'running'`, agent)
	if err != nil {
		return err
	}
	defer rows.Close()
	target := jid.ToNonAD()
	for rows.Next() {
		var chat, requester string
		if err := rows.Scan(&chat, &requester); err != nil {
			return err
		}
		number, _, _ := strings.Cut(requester, "\x00")
		same := chat == target.String()
		if o, err := types.ParseJID(chat); err == nil && o.ToNonAD() == target {
			same = true
		}
		if !strings.HasSuffix(chat, "@g.us") && target.Server == types.DefaultUserServer && number != "" && target.User == strings.TrimPrefix(number, "+") {
			same = true
		}
		if same {
			return toolErr("an update for this chat is being prepared: it goes as that turn's reply, not through whatsapp_send")
		}
	}
	return rows.Err()
}
