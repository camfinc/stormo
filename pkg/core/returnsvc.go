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
// is a return. What comes back about it is captured, in the transaction that creates it, as the
// return's event, and brings the agent back into that chat through a delivery: a channel turn whose
// reply the core posts there as one message through a journaled outbox. The state machine is
// returns.go; this is its database and the bus's side; the worker (returnworker.go) drives the
// deliveries.

// ReturnPoster is the agent's channel as the returns post to it (the WhatsApp connector).
type ReturnPoster interface {
	AllowedReply(o whatsapp.Origin) error
	PostOrigin(ctx context.Context, o whatsapp.Origin, text string, journal func(state, id string) error) error
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
	started  time.Time  // when this core's return paths began: turns older are a stopped core's
	// sentIDs are sends that went but whose settlement could not be written (under mu): written
	// again each round, never sent again.
	sentIDs map[int64]string
}

// capsEntry is an agent's negotiation: the last answer its engine gave (answer: when; zero, never)
// and when it was last asked. An engine that cannot be reached keeps its last answer: a restart
// must not read as the engine no longer running channel turns.
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
	r := &Returns{o: o, kick: make(chan struct{}, 1), started: o.Now(), sentIDs: map[int64]string{}}
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

// draining is why agent's returns close ("" when they do not): the agent opted out, the instance
// drains, or its engine answered it no longer enforces channel turns.
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
// answering it can come back): its answer; ErrNoReturns when the turn is not one (no origin). When
// ctx ends first the turn is abandoned: the connector tells the person it could not complete, and
// the worker follows the turn to its end so nothing it did is forgotten, but posts nothing.
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
	in := engine.Input{Text: t.Text, AudioFormat: "opus", Files: turnFiles(t)}
	turn := engine.ChannelTurn{Execution: exec, Token: token, Kind: "inbound", Session: session, In: in}
	st, err := r.admit(ctx, ct, turn)
	if err == nil {
		st, err = r.await(ctx, ct, exec, st)
	}
	switch {
	case ctx.Err() != nil:
		_, _ = r.o.DB.Exec(`UPDATE executions SET state = 'abandoned' WHERE id = ? AND ended IS NULL`, exec)
		r.Kick()
		return whatsapp.Answer{}, fmt.Errorf("the turn did not end in time: %w", ctx.Err())
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

// recoveryReads are the core tools an update's recovery turn may call: it reads and reports.
var recoveryReads = map[string]bool{"msg_inbox": true, "msg_read": true, "msg_thread": true}

// guard refuses a recovery turn's call to anything but a read (its runtime offers it nothing
// else; this holds even when the runtime does not).
func (r *Returns) guard(tool string, call ToolCall) error {
	exec := r.Verified(call.Agent, call.Meta.Exec, call.Meta.ExecToken)
	if exec == "" || recoveryReads[tool] {
		return nil
	}
	var kind string
	if r.o.DB.QueryRow(`SELECT kind FROM executions WHERE id = ?`, exec).Scan(&kind) == nil && kind == "recovery" {
		return toolErr("this turn only reads and reports: %s is not available in it", tool)
	}
	return nil
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
// call naming an execution the core did not start, another agent's, one without its token, or a
// recovery turn's; a thread that ever carried another person's request. A send from no execution
// has none.
func (r *Returns) sendOrigin(tx *sql.Tx, sender, exec, token string, thread int64) (sendFrom, error) {
	var f sendFrom
	if exec == "" {
		return f, nil
	}
	var agent, kind, state, tok string
	var ret sql.NullInt64
	err := tx.QueryRow(`SELECT agent, kind, state, origin, token, ret FROM executions WHERE id = ?`, exec).Scan(&agent, &kind, &state, &f.origin, &tok, &ret)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return f, toolErr("this call names a turn the core did not start")
	case err != nil:
		return f, err
	case agent != sender:
		return f, toolErr("this turn's execution is not yours")
	case tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(token)) != 1:
		return f, toolErr("this call does not carry its turn's token")
	case kind == "recovery":
		return f, toolErr("this turn only reads and reports: it sends no messages")
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
		why := MayDelegate(v, r.o.Now())
		if state == "retired" {
			why = "the update's turn it comes from was given up" // still running, it opens no path to the person
		}
		if why != "" {
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

// capture records message's arrival as an event of return ret, in the transaction that created it,
// and takes the message's delivery to the return's agent (deliveryAgent) from the bus, for good: the
// return brings it to the person. A closed return captures nothing: the message stays the bus's.
func (r *Returns) capture(tx *sql.Tx, ret int64, kind string, message int64, deliveryAgent string) error {
	v, err := r.view(tx, ret)
	if err != nil || v.Terminal() {
		return err
	}
	seq := v.SeqLast + 1
	if _, err := tx.Exec(`INSERT INTO return_events(ret, seq, kind, message, at) VALUES(?,?,?,?,?)`, ret, seq, kind, message, r.o.Now().UnixMilli()); err != nil {
		return err
	}
	if message != 0 && deliveryAgent != "" {
		if _, err := tx.Exec(`UPDATE deliveries SET owner = 'return', return_id = ? WHERE message = ? AND agent = ? AND owner = 'bus'`,
			ret, message, deliveryAgent); err != nil {
			return err
		}
	}
	if err := r.apply(tx, ret, Capture(v, kind, seq, r.o.Now())); err != nil {
		return err
	}
	r.Kick()
	return nil
}

// onSend captures what a stored message brings: an answer to the request it names, or progress on
// the one request of its thread. An answer naming requests made for different people (closed or
// open: a closed return still says whose a request was) or for more than one open return is refused.
func (r *Returns) onSend(tx *sql.Tx, msg int64, sender string, recipients []BusAgent, thread int64, answers []int64) error {
	type hit struct {
		ret   int64
		agent string
	}
	seen := map[int64]hit{}
	people := map[string]bool{}
	for _, a := range answers {
		var h hit
		var origin string
		var closed sql.NullInt64
		err := tx.QueryRow(`SELECT id, agent, origin, closed_at FROM returns WHERE request = ? AND recipient = ?`, a, sender).Scan(&h.ret, &h.agent, &origin, &closed)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		people[origin] = true
		if !closed.Valid {
			seen[h.ret] = h
		}
	}
	if len(people) > 1 {
		return toolErr("this message answers requests made for different people: answer each in a message of its own")
	}
	if len(seen) > 1 {
		return toolErr("this message answers requests the core follows separately: answer each in a message of its own")
	}
	for _, h := range seen {
		return r.capture(tx, h.ret, EventAnswered, msg, h.agent)
	}
	if len(answers) > 0 || thread == 0 || len(recipients) != 1 {
		return nil
	}
	rows, err := tx.Query(`SELECT id FROM returns WHERE thread = ? AND recipient = ? AND agent = ? AND closed_at IS NULL`, thread, sender, recipients[0].ID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	switch len(ids) {
	case 0:
	case 1:
		return r.capture(tx, ids[0], EventProgress, msg, recipients[0].ID)
	default:
		// it could be about more than one request: it stays the bus's, never routed
		raiseFindingTx(tx, sender, "return_ambiguous", "medium", fmt.Sprintf("message %d", msg), r.o.Now().UnixMilli())
	}
	return nil
}

// followed is whether request (sent by sender) has an open return: the bus's own "still open" and
// "past its due time" notices are not sent for it (the return's due prompts tell the person).
func (r *Returns) followed(tx *sql.Tx, request int64, sender string) bool {
	var n int
	return tx.QueryRow(`SELECT COUNT(*) FROM returns WHERE request = ? AND agent = ? AND closed_at IS NULL`, request, sender).Scan(&n) == nil && n > 0
}

// onFailure captures the bus's "Not answered" notice about request as its return's failure event.
func (r *Returns) onFailure(tx *sql.Tx, request, notice int64, sender string) error {
	var ret int64
	err := tx.QueryRow(`SELECT id FROM returns WHERE request = ? AND agent = ? AND closed_at IS NULL`, request, sender).Scan(&ret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.capture(tx, ret, EventFailure, notice, sender)
}

// raiseFindingTx is DB.RaiseFinding inside a transaction (core.db has one connection).
func raiseFindingTx(tx querier, agent, rule, severity, evidence string, at int64) {
	_, _ = tx.Exec(`INSERT INTO findings(agent, rule, severity, evidence, first_seen, last_seen) VALUES(?,?,?,?,?,?)
		ON CONFLICT(agent, rule, evidence) DO UPDATE SET last_seen = excluded.last_seen, count = count + 1, severity = excluded.severity,
		status = CASE WHEN status = 'resolved' THEN 'open' ELSE status END`, agent, rule, severity, evidence, at, at)
}

// sideDoor refuses agent's whatsapp_send to a chat one of its update turns is running for, whatever
// the call claims of itself: an update to the person goes through the outbox (the turn's reply),
// never around it. A chat is the person it is: a group's id, or a direct chat's number however the
// origin addressed it (a group's requester written to directly is another chat).
func (r *Returns) sideDoor(agent string, jid types.JID) error {
	rows, err := r.o.DB.Query(`SELECT o.chat, o.requester FROM executions x JOIN origins o ON o.id = x.origin
		WHERE x.agent = ? AND x.kind IN ('return', 'recovery') AND x.ended IS NULL`, agent)
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
