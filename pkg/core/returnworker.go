package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/whatsapp"
)

// The return paths' worker: it ticks the returns (escalation, deadlines, expiry, closing what
// drains), drives each delivery through the outbox (queued → running → prepared → sending →
// settled), and follows abandoned person's turns to their end. A delivery is a channel turn in the
// origin's conversation whose reply is posted there as one message: journaled "sending" before it
// goes, settled confirmed after, and never sent again once it may have gone.

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Exec(query string, args ...any) (sql.Result, error)
}

func ms(t int64) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.UnixMilli(t)
}

func unms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// view is return id as its state machine sees it.
func (r *Returns) view(q querier, id int64) (ReturnView, error) {
	var v ReturnView
	var active, escalated, noDue int
	var escAt, escalatedAt, nextDue, last int64
	err := q.QueryRow(`SELECT state, seq_last, seq_settled, active, active_from, active_to, depth, escalated, no_due,
		escalate_at, escalated_at, next_due, last_activity, close_reason FROM returns WHERE id = ?`, id).Scan(&v.State, &v.SeqLast, &v.SeqSettled,
		&active, &v.ActiveFrom, &v.ActiveTo, &v.Depth, &escalated, &noDue, &escAt, &escalatedAt, &nextDue, &last, &v.CloseReason)
	v.Active, v.Escalated, v.NoDue = active != 0, escalated != 0, noDue != 0
	v.EscalateAt, v.EscalatedAt, v.NextDue, v.LastActivity = ms(escAt), ms(escalatedAt), ms(nextDue), ms(last)
	return v, err
}

// save writes v back to return id; a closed state closes it (closed_at, once).
func (r *Returns) save(q querier, id int64, v ReturnView) error {
	_, err := q.Exec(`UPDATE returns SET state = ?, seq_last = ?, seq_settled = ?, active = ?, active_from = ?, active_to = ?, escalated = ?,
		escalated_at = ?, no_due = ?, next_due = ?, last_activity = ?, close_reason = ?,
		closed_at = CASE WHEN ? = 1 THEN COALESCE(closed_at, ?) ELSE NULL END WHERE id = ?`,
		v.State, v.SeqLast, v.SeqSettled, v.Active, v.ActiveFrom, v.ActiveTo, v.Escalated, unms(v.EscalatedAt), v.NoDue,
		unms(v.NextDue), unms(v.LastActivity), v.CloseReason, btoi(v.Terminal()), unms(r.o.Now()), id)
	return err
}

// apply saves a step of return id and carries out its actions, in the caller's transaction.
func (r *Returns) apply(tx *sql.Tx, id int64, step Step) error {
	if err := r.save(tx, id, step.Next); err != nil {
		return err
	}
	for _, a := range step.Actions {
		if err := r.act(tx, id, a); err != nil {
			return fmt.Errorf("returns: %s %d: %w", a.Kind, id, err)
		}
	}
	return nil
}

// inTx loads return id, steps its machine with f and applies the step, in one transaction: all of
// it, or none.
func (r *Returns) inTx(id int64, f func(*sql.Tx, ReturnView) (Step, error)) error {
	tx, err := r.o.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v, err := r.view(tx, id)
	if err != nil {
		return err
	}
	step, err := f(tx, v)
	if err != nil {
		return err
	}
	if err := r.apply(tx, id, step); err != nil {
		log.Print(err)
		return err
	}
	return tx.Commit()
}

// act carries out an action inside the transaction.
func (r *Returns) act(tx *sql.Tx, id int64, a Action) error {
	now := r.o.Now()
	var agent string
	if err := tx.QueryRow(`SELECT agent FROM returns WHERE id = ?`, id).Scan(&agent); err != nil {
		return err
	}
	switch a.Kind {
	case ActEnqueue:
		_, err := tx.Exec(`INSERT INTO return_deliveries(ret, seq_from, seq_to, event, kind, state, created, updated) VALUES(?,?,?,?,'update','queued',?,?)`,
			id, a.From, a.To, a.Event, now.UnixMilli(), now.UnixMilli())
		return err
	case ActSchedule:
		if set := r.o.Settings(agent); set != nil {
			sched, loc := set.Schedule()
			_, err := tx.Exec(`UPDATE returns SET next_due = ? WHERE id = ?`, unms(sched.AddWorking(now, set.UpdateAfter, loc)), id)
			return err
		}
	case ActEscalate:
		raiseFindingTx(tx, agent, "return_escalated", "high", fmt.Sprintf("return %d", id), now.UnixMilli())
	case ActFinding:
		evidence := fmt.Sprintf("return %d", id)
		if a.Reason != "" {
			evidence += ": " + clip(a.Reason, 200)
		}
		raiseFindingTx(tx, agent, a.Rule, a.Severity, evidence, now.UnixMilli())
	case ActClose:
		// the active delivery settles as closed; its turn, if any, is over for the core
		var exec string
		if err := tx.QueryRow(`SELECT execution FROM return_deliveries WHERE ret = ? AND state != 'settled'`, id).Scan(&exec); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE return_deliveries SET state = 'settled', outcome = ?, updated = ? WHERE ret = ? AND state != 'settled'`,
			OutcomeClosed, now.UnixMilli(), id); err != nil {
			return err
		}
		if exec != "" {
			_, err := tx.Exec(`UPDATE executions SET state = 'failed', error = 'the return closed', ended = ? WHERE id = ? AND ended IS NULL`, now.UnixMilli(), exec)
			return err
		}
	}
	return nil
}

// Run is the worker: a round every interval and on every Kick, until stop closes.
func (r *Returns) Run(stop <-chan struct{}, interval time.Duration) {
	r.recover()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-r.kick:
		}
		r.Round()
	}
}

// recover settles what a stopped core left: an update being sent may or may not have gone
// (unconfirmed, never sent again); a person's turn the connector was waiting for has no one waiting
// now (abandoned: followed to its end, nothing posted). Only turns older than this core's start: a
// live connector's are its own.
func (r *Returns) recover() {
	_, _ = r.o.DB.Exec(`UPDATE executions SET state = 'abandoned' WHERE kind = 'inbound' AND state = 'running' AND ended IS NULL AND created < ?`, unms(r.started))
	for _, d := range r.deliveries(`d.state = 'sending'`) {
		_ = r.settle(d, "sending", OutcomeUnconfirmed, "", "the core stopped while it was being sent")
	}
}

// Round is one pass: the agents' negotiations kept fresh, the returns ticked, abandoned turns
// followed, the deliveries driven, and once an hour what is old scrubbed.
func (r *Returns) Round() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, agent := range r.agents() {
		r.negotiated(agent) // asked again once its answer is a TTL old: a request opens on a fresh one
	}
	r.tick()
	r.abandoned()
	r.drive()
	if now := r.o.Now(); now.Sub(r.scrubbed) >= time.Hour {
		r.scrubbed = now
		r.scrub()
	}
}

// agents are those with a return open or a turn not ended.
func (r *Returns) agents() []string {
	rows, err := r.o.DB.Query(`SELECT agent FROM returns WHERE closed_at IS NULL UNION SELECT agent FROM executions WHERE ended IS NULL`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if rows.Scan(&a) == nil {
			out = append(out, a)
		}
	}
	return out
}

// tick closes the open returns whose agent drains, and runs the deadlines of the others. A return
// whose update is being sent is left until that send settles.
func (r *Returns) tick() {
	type ret struct {
		id      int64
		agent   string
		sending int
	}
	rows, err := r.o.DB.Query(`SELECT x.id, x.agent, (SELECT COUNT(*) FROM return_deliveries d WHERE d.ret = x.id AND d.state = 'sending')
		FROM returns x WHERE x.closed_at IS NULL`)
	if err != nil {
		return
	}
	var rs []ret
	for rows.Next() {
		var x ret
		if rows.Scan(&x.id, &x.agent, &x.sending) == nil {
			rs = append(rs, x)
		}
	}
	rows.Close()
	for _, x := range rs {
		if x.sending > 0 {
			continue
		}
		if why := r.draining(x.agent); why != "" {
			r.closeReturn(x.id, why)
			continue
		}
		_ = r.inTx(x.id, func(tx *sql.Tx, v ReturnView) (Step, error) { return Tick(v, r.o.Now()), nil })
	}
}

// closeReturn closes return id with reason: everything captured settled, the owner told.
func (r *Returns) closeReturn(id int64, reason string) {
	_ = r.inTx(id, func(tx *sql.Tx, v ReturnView) (Step, error) { return Close(v, reason, r.o.Now()), nil })
}

// abandoned follows the person's turns the connector stopped waiting for to their end: what they
// did is recorded and their result taken from the runtime; nothing is posted (the person was told
// it could not complete).
func (r *Returns) abandoned() {
	rows, err := r.o.DB.Query(`SELECT id, agent FROM executions WHERE kind = 'inbound' AND state = 'abandoned' AND ended IS NULL`)
	if err != nil {
		return
	}
	type ex struct{ id, agent string }
	var xs []ex
	for rows.Next() {
		var x ex
		if rows.Scan(&x.id, &x.agent) == nil {
			xs = append(xs, x)
		}
	}
	rows.Close()
	for _, x := range xs {
		ct, err := r.o.Channel(x.agent)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		st, err := ct.Status(ctx, x.id, 0)
		switch {
		case errors.Is(err, engine.ErrChannelUnknown):
			r.endExecution(x.id, "failed", 0, "abandoned; the engine lost the turn")
		case err == nil && st.Terminal():
			r.endExecution(x.id, st.State, st.Acted, "abandoned: the person was told it could not complete")
			_ = ct.Take(ctx, x.id)
		}
		cancel()
	}
}

// ---- deliveries ----------------------------------------------------------------------------

type delivery struct {
	id, ret, from, to     int64
	event, kind, state    string
	execution, input      string
	started               int64
	turns                 int
	acted                 int64
	disposition, prepared string
	truncated             bool
	postTries             int
	nextTry               int64
	agent, origin         string
	request, thread       int64
	recipient             string
}

// deliveries are those matching where (on d, the delivery), with their return's.
func (r *Returns) deliveries(where string) []delivery {
	rows, err := r.o.DB.Query(`SELECT d.id, d.ret, d.seq_from, d.seq_to, d.event, d.kind, d.state, d.execution, d.input, d.started, d.turns, d.acted,
		d.disposition, d.prepared, d.truncated, d.post_tries, d.next_try, x.agent, x.origin, COALESCE(x.request, 0), x.thread, x.recipient
		FROM return_deliveries d JOIN returns x ON x.id = d.ret WHERE ` + where + ` ORDER BY d.id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var ds []delivery
	for rows.Next() {
		var d delivery
		if rows.Scan(&d.id, &d.ret, &d.from, &d.to, &d.event, &d.kind, &d.state, &d.execution, &d.input, &d.started, &d.turns, &d.acted,
			&d.disposition, &d.prepared, &d.truncated, &d.postTries, &d.nextTry, &d.agent, &d.origin, &d.request, &d.thread, &d.recipient) == nil {
			ds = append(ds, d)
		}
	}
	return ds
}

// startPatience is how long a delivery's turn may go unconfirmed (the runtime never answering
// whether it admitted it) before it is given up: it may have run, so it is never run again.
const startPatience = 30 * time.Minute

// drive moves every delivery not being sent one step. Nothing moves while the instance's switch is
// stop: it is all held as it is.
func (r *Returns) drive() {
	if r.o.Mode() == "stop" {
		return
	}
	now := r.o.Now().UnixMilli()
	for _, d := range r.deliveries(`d.state IN ('queued', 'running', 'prepared')`) {
		if d.nextTry > now {
			continue
		}
		switch d.state {
		case "queued":
			r.startDelivery(d)
		case "running":
			r.pollDelivery(d)
		case "prepared":
			r.post(d)
		}
	}
	for id, sentID := range r.sentIDs {
		// a send that went but whose settlement could not be written: written again, never resent
		for _, d := range r.deliveries(fmt.Sprintf(`d.id = %d AND d.state = 'sending'`, id)) {
			if r.settle(d, "sending", OutcomeConfirmed, sentID, "") == nil {
				delete(r.sentIDs, id)
			}
		}
	}
}

// maxUpdate is the most runes an update holds: one WhatsApp message, cut short past it.
const maxUpdate = 3500

// continuation is the id-only input of a delivery's turn: what happened, where to read it, never
// the reply's words (reading them with msg_read marks the run as any read does).
func (r *Returns) continuation(d delivery, requester string) string {
	rows, err := r.o.DB.Query(`SELECT kind, message FROM return_events WHERE ret = ? AND seq BETWEEN ? AND ? ORDER BY seq`, d.ret, d.from, d.to)
	var what, read []string
	if err == nil {
		for rows.Next() {
			var kind string
			var msg int64
			if rows.Scan(&kind, &msg) != nil {
				continue
			}
			switch kind {
			case EventAnswered:
				what = append(what, fmt.Sprintf("%s answered", d.recipient))
			case EventProgress:
				what = append(what, fmt.Sprintf("%s wrote, without marking it the answer", d.recipient))
			case EventFailure:
				what = append(what, "the bus says it went unanswered")
			}
			if msg != 0 {
				read = append(read, fmt.Sprint(msg))
			}
		}
		rows.Close()
	}
	if len(what) == 0 {
		what = append(what, "they have had no update for a while")
	}
	readLine := ""
	if len(read) > 0 {
		readLine = fmt.Sprintf(" Read %s with msg_read.", strings.Join(read, ", "))
	}
	return fmt.Sprintf("[RETURN] News on your request %d to %s, which you sent for %s in this chat: %s.%s "+
		"Tell them, in this chat, what they need to know now, in one message of under 3000 characters. End your reply with "+
		"one line: [DONE] if it gives them what they asked for, [UPDATE] if it is progress, or [SILENT] if there is nothing "+
		"to tell them yet.", d.request, d.recipient, orElse(requester, "the person"), strings.Join(what, "; "), readLine)
}

// recoveryInput is a recovery turn's input: the update's turn failed, maybe after acting; this one
// only reads (its runtime offers it nothing else) and reports.
func (r *Returns) recoveryInput(d delivery, requester string) string {
	return fmt.Sprintf("[RETURN RECOVERY] The turn that was to update %s about your request %d to %s failed after %d tool call(s): "+
		"it may have done part of what it set out to. This turn can only read. Do not promise to repeat anything. Read the "+
		"request's thread with msg_thread(%d) to see where it stands, and tell them, in this chat, in one message of under "+
		"3000 characters. End your reply with one line: [DONE], [UPDATE] or [SILENT].",
		orElse(requester, "the person"), d.request, d.recipient, d.acted, d.thread)
}

func orElse(s, alt string) string {
	if strings.TrimSpace(s) == "" {
		return alt
	}
	return s
}

// startDelivery runs a delivery's turn: an execution in the origin's conversation (kind return, or
// recovery). Its execution, token and input are kept before the runtime is asked, so an answer that
// never came is asked again with the very same request, which the runtime admits once.
func (r *Returns) startDelivery(d delivery) {
	ct, err := r.o.Channel(d.agent)
	if err != nil {
		r.closeReturn(d.ret, "the agent's channel turns are not available")
		return
	}
	now := r.o.Now()
	kind := "return"
	if d.kind == "recovery" {
		kind = "recovery"
	}
	if d.execution == "" {
		var session string
		if r.o.DB.QueryRow(`SELECT session FROM origins WHERE id = ?`, d.origin).Scan(&session) != nil {
			r.closeReturn(d.ret, "the origin is gone")
			return
		}
		_, requester, _ := r.originOf(d.origin)
		input := r.continuation(d, requester)
		if kind == "recovery" {
			input = r.recoveryInput(d, requester)
		}
		exec, token := newID(), newToken()
		tx, err := r.o.DB.Begin()
		if err != nil {
			return
		}
		_, err = tx.Exec(`INSERT INTO executions(id, agent, kind, origin, token, ret, session, state, created) VALUES(?,?,?,?,?,?,?,?,?)`,
			exec, d.agent, kind, d.origin, token, d.ret, session, "running", now.UnixMilli())
		var res sql.Result
		if err == nil {
			res, err = tx.Exec(`UPDATE return_deliveries SET execution = ?, input = ?, started = ?, updated = ? WHERE id = ? AND state = 'queued' AND execution = ''`,
				exec, input, now.UnixMilli(), now.UnixMilli(), d.id)
		}
		if n, _ := rowsOf(res); err != nil || n == 0 || tx.Commit() != nil {
			_ = tx.Rollback()
			return
		}
		d.execution, d.input, d.started = exec, input, now.UnixMilli()
	}
	var token, session string
	if r.o.DB.QueryRow(`SELECT token, session FROM executions WHERE id = ?`, d.execution).Scan(&token, &session) != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = ct.Start(ctx, engine.ChannelTurn{Execution: d.execution, Token: token, Kind: kind, Session: session, In: engine.Input{Text: d.input}})
	switch {
	case err == nil:
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET state = 'running', error = '', updated = ? WHERE id = ? AND state = 'queued'`, now.UnixMilli(), d.id)
	case errors.Is(err, engine.ErrChannelBusy):
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET next_try = ? WHERE id = ?`, now.Add(15*time.Second).UnixMilli(), d.id)
	case errors.Is(err, engine.ErrChannelRefused):
		r.endExecution(d.execution, "failed", 0, err.Error())
		r.turnFailed(d, 0, err.Error())
	case now.Sub(ms(d.started)) >= startPatience:
		// never confirmed either way: it may have run, and it cannot be shown to have stopped
		r.endExecution(d.execution, "failed", 0, "never confirmed: "+err.Error())
		_ = r.settle(d, "queued", OutcomeFailed, "", "the agent's engine never confirmed the update's turn")
	default:
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET next_try = ?, error = ? WHERE id = ?`, now.Add(30*time.Second).UnixMilli(), clip(err.Error(), 300), d.id)
	}
}

func rowsOf(res sql.Result) (int64, error) {
	if res == nil {
		return 0, nil
	}
	return res.RowsAffected()
}

// turnFailed is a delivery's turn ending in failure, after acted tool calls (this turn's): a turn
// that provably did nothing runs again with a new execution (ReturnTurnTries in all); one that may
// have acted, or the last try, gets one recovery turn; a recovery turn that fails settles it as
// failed.
func (r *Returns) turnFailed(d delivery, acted int64, why string) {
	now := r.o.Now()
	switch {
	case d.kind == "update" && acted == 0 && d.turns+1 < ReturnTurnTries:
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET state = 'queued', execution = '', input = '', started = 0, turns = turns + 1, error = ?,
			next_try = ?, updated = ? WHERE id = ? AND state IN ('queued', 'running')`, clip(why, 300), now.Add(10*time.Second).UnixMilli(), now.UnixMilli(), d.id)
	case d.kind == "update":
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET kind = 'recovery', recovery_used = 1, acted = acted + ?, state = 'queued', execution = '',
			input = '', started = 0, error = ?, next_try = ?, updated = ? WHERE id = ? AND state IN ('queued', 'running') AND recovery_used = 0`,
			acted, clip(why, 300), now.Add(10*time.Second).UnixMilli(), now.UnixMilli(), d.id)
	default:
		_ = r.settle(d, d.state, OutcomeFailed, "", "the update's turn and its recovery failed: "+why)
	}
}

// Disposition markers, each alone on a line.
var markers = map[string]string{"[DONE]": DispositionDone, "[UPDATE]": DispositionUpdate, "[SILENT]": DispositionSilent}

// disposition reads a reply's control line and the reply without its markers. The disposition is
// its last line's when that line is a marker alone and no other marker contradicts it; anything
// else (no marker, a marker inside a line, two different markers) is an update. A reply with
// nothing left to say is silent.
func disposition(text string) (string, string) {
	lines := strings.Split(strings.TrimRight(text, " \t\r\n"), "\n")
	var kept []string
	seen := map[string]bool{}
	last := ""
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if d, ok := markers[t]; ok {
			seen[d] = true
			if i == len(lines)-1 {
				last = d
			}
			continue
		}
		for m := range markers {
			if strings.Contains(l, m) { // a marker run into the text: dropped, it decides nothing
				l = strings.TrimRight(strings.ReplaceAll(strings.ReplaceAll(l, " "+m, ""), m, ""), " \t\r")
				seen["inline"] = true
			}
		}
		kept = append(kept, l)
	}
	rest := strings.TrimSpace(strings.Join(kept, "\n"))
	disp := DispositionUpdate
	if last != "" && len(seen) == 1 {
		disp = last
	}
	if disp == DispositionSilent || rest == "" {
		return DispositionSilent, ""
	}
	return disp, rest
}

// fit is text as one message: at most maxUpdate runes, cut short (and said so) past it.
func fit(text string) (string, bool) {
	rs := []rune(text)
	if len(rs) <= maxUpdate {
		return text, false
	}
	return strings.TrimRight(string(rs[:maxUpdate-1]), " \t\r\n") + "…", true
}

// pollDelivery asks about a delivery's turn; once it ended, its reply is kept (prepared) as one
// message, or its failure handled. A turn the runtime no longer knows may have acted and cannot be
// shown to have stopped: settled as failed, never rerun or recovered.
func (r *Returns) pollDelivery(d delivery) {
	ct, err := r.o.Channel(d.agent)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := ct.Status(ctx, d.execution, 0)
	if errors.Is(err, engine.ErrChannelUnknown) {
		r.endExecution(d.execution, "failed", 0, "the engine lost the turn")
		_ = r.settle(d, "running", OutcomeFailed, "", "the agent's engine lost the update's turn; it may have acted")
		return
	}
	if err != nil || !st.Terminal() {
		return
	}
	r.endExecution(d.execution, st.State, st.Acted, st.Error)
	if st.State != "ok" {
		_ = ct.Take(ctx, d.execution)
		r.turnFailed(d, st.Acted, st.Error)
		return
	}
	disp, text := disposition(st.Text)
	if disp == DispositionSilent {
		_ = ct.Take(ctx, d.execution)
		d.disposition = DispositionSilent
		_ = r.settle(d, "running", OutcomeSilent, "", "")
		return
	}
	msg, truncated := fit(text)
	mark := ""
	if st.Marked {
		mark = "marked"
	}
	res, err := r.o.DB.Exec(`UPDATE return_deliveries SET state = 'prepared', disposition = ?, full_text = ?, prepared = ?, truncated = ?, prepared_mark = ?,
		post_tries = 0, next_try = 0, updated = ? WHERE id = ? AND state = 'running'`, disp, text, msg, truncated, mark, r.o.Now().UnixMilli(), d.id)
	if n, _ := rowsOf(res); err == nil && n > 0 {
		_ = ct.Take(ctx, d.execution) // kept here now: the engine may forget it
	}
}

// errFenced is the journal refusing the send: the switch is stop, the agent's returns drain, or the
// delivery is no longer prepared. Nothing went.
var errFenced = errors.New("returns: the post was stopped")

// post sends a prepared delivery's message to the origin: journaled sending before it goes,
// settled confirmed with its WhatsApp id after. It is sent again only when it provably did not go;
// one that may have gone settles as unconfirmed.
func (r *Returns) post(d delivery) {
	p, err := r.o.Poster(d.agent)
	if err != nil {
		r.retryPost(d, err)
		return
	}
	o, _, err := r.originOf(d.origin)
	if err != nil {
		r.closeReturn(d.ret, "the origin is gone")
		return
	}
	if err := p.AllowedReply(o); err != nil {
		if errors.Is(err, whatsapp.ErrNotConnected) {
			r.retryPost(d, err)
		} else {
			r.closeReturn(d.ret, err.Error())
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err = p.PostOrigin(ctx, o, d.prepared, func(state, id string) error {
		if state == "sending" {
			return r.fence(d)
		}
		return r.settle(d, "sending", OutcomeConfirmed, id, "")
	})
	var pe *whatsapp.PostError
	switch {
	case err == nil:
	case errors.As(err, &pe) && pe.Sent:
		// it went: its settlement is written again, every round until it is, never the send
		if r.settle(d, "sending", OutcomeConfirmed, pe.ID, "") != nil {
			r.sentIDs[d.id] = pe.ID
		}
	case errors.As(err, &pe) && pe.Unsent && errors.Is(err, errFenced):
		// stopped before it went: the next round decides (held, or closed as it drains)
	case errors.As(err, &pe) && pe.Unsent:
		res, _ := r.o.DB.Exec(`UPDATE return_deliveries SET state = 'prepared', updated = ? WHERE id = ? AND state = 'sending'`, r.o.Now().UnixMilli(), d.id)
		if n, _ := rowsOf(res); n == 0 && !errors.Is(err, whatsapp.ErrNotConnected) {
			r.closeReturn(d.ret, pe.Err.Error()) // no longer allowed to post there
			return
		}
		r.retryPost(d, err)
	default:
		_ = r.settle(d, "sending", OutcomeUnconfirmed, "", err.Error())
	}
}

// fence is the journal's check before the message goes, under the posting lane: the switch is not
// stop, the agent's returns do not drain, and the delivery is still prepared; then it is marked
// sending.
func (r *Returns) fence(d delivery) error {
	if r.o.Mode() == "stop" || r.draining(d.agent) != "" {
		return errFenced
	}
	res, err := r.o.DB.Exec(`UPDATE return_deliveries SET state = 'sending', updated = ? WHERE id = ? AND state = 'prepared'`, r.o.Now().UnixMilli(), d.id)
	if err != nil {
		return fmt.Errorf("%w: %v", errFenced, err) // not marked: it does not go
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errFenced
	}
	return nil
}

// postTries is how often a prepared update is tried, when it provably did not go, before it settles
// as failed.
const postTries = 6

// retryPost tries a post again later (a minute more each time), or gives up.
func (r *Returns) retryPost(d delivery, cause error) {
	if d.postTries+1 < postTries {
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET post_tries = post_tries + 1, next_try = ?, error = ? WHERE id = ?`,
			r.o.Now().Add(time.Duration(d.postTries+1)*time.Minute).UnixMilli(), clip(cause.Error(), 300), d.id)
		return
	}
	_ = r.settle(d, "prepared", OutcomeFailed, "", "it could not be posted: "+cause.Error())
}

// settle closes delivery d (in state from) with its outcome and feeds it to its return's machine,
// in one transaction; its turn is over for the core. A delivery that moved on is left alone.
func (r *Returns) settle(d delivery, from, outcome, sentID, reason string) error {
	return r.inTx(d.ret, func(tx *sql.Tx, v ReturnView) (Step, error) {
		now := r.o.Now().UnixMilli()
		res, err := tx.Exec(`UPDATE return_deliveries SET state = 'settled', outcome = ?, sent_id = ?, error = ?, updated = ? WHERE id = ? AND state = ?`,
			outcome, sentID, clip(reason, 300), now, d.id, from)
		if err != nil {
			return Step{}, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return Step{}, errMovedOn
		}
		if d.execution != "" {
			if _, err := tx.Exec(`UPDATE executions SET state = CASE WHEN state = 'running' THEN 'failed' ELSE state END, ended = ? WHERE id = ? AND ended IS NULL`,
				now, d.execution); err != nil {
				return Step{}, err
			}
		}
		return Finish(v, DeliveryResult{From: d.from, To: d.to, Outcome: outcome, Disposition: d.disposition, Truncated: d.truncated, Reason: reason}, r.o.Now()), nil
	})
}

// errMovedOn is a step whose delivery moved on meanwhile: nothing is saved.
var errMovedOn = errors.New("returns: the delivery moved on")

// returnKeep is how long what a settled delivery said is kept: its words and the person's quote go
// after it, its states and ids stay (a thread stays bound to its person).
const returnKeep = 30 * 24 * time.Hour

// scrub clears what is old: the words of settled deliveries, the quotes and requesters of origins
// nothing open needs, and the tokens of executions ended a week ago (a call that late is refused).
func (r *Returns) scrub() {
	now := r.o.Now()
	cut := now.Add(-returnKeep).UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE return_deliveries SET prepared = '', full_text = '', input = '' WHERE updated < ? AND state = 'settled'
			AND (prepared != '' OR full_text != '' OR input != '')`, []any{cut}},
		{`UPDATE origins SET quote = '', requester = '' WHERE created < ? AND (quote != '' OR requester != '')
			AND id NOT IN (SELECT origin FROM returns WHERE closed_at IS NULL) AND id NOT IN (SELECT origin FROM executions WHERE ended IS NULL)`, []any{cut}},
		{`UPDATE executions SET token = '' WHERE ended IS NOT NULL AND ended < ? AND token != ''`, []any{now.Add(-7 * 24 * time.Hour).UnixMilli()}},
	} {
		if _, err := r.o.DB.Exec(q.sql, q.args...); err != nil {
			log.Printf("returns: scrub: %v", err)
		}
	}
}

// ReturnRow is a return as the owner's diagnostics list it: states, counts and times, never a
// requester or a word of what was said.
type ReturnRow struct {
	ID          int64  `json:"id"`
	Agent       string `json:"agent"`
	Recipient   string `json:"recipient"`
	State       string `json:"state"`
	CloseReason string `json:"close_reason,omitempty"`
	Request     int64  `json:"request"`
	Thread      int64  `json:"thread"`
	Escalated   bool   `json:"escalated"`
	Created     string `json:"created"`
	Closed      string `json:"closed,omitempty"`
	// Settled updates by outcome.
	Confirmed   int `json:"confirmed"`
	Unconfirmed int `json:"unconfirmed"`
	Silent      int `json:"silent"`
	Failed      int `json:"failed"`
	Truncated   int `json:"truncated"`
}

// List is the returns, newest first (agent "" all), at most limit.
func (r *Returns) List(agent string, limit int) ([]ReturnRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.o.DB.Query(`SELECT x.id, x.agent, x.recipient, x.state, x.close_reason, COALESCE(x.request, 0), x.thread, x.escalated, x.created,
		COALESCE(x.closed_at, 0),
		(SELECT COUNT(*) FROM return_deliveries d WHERE d.ret = x.id AND d.outcome = 'confirmed'),
		(SELECT COUNT(*) FROM return_deliveries d WHERE d.ret = x.id AND d.outcome = 'unconfirmed'),
		(SELECT COUNT(*) FROM return_deliveries d WHERE d.ret = x.id AND d.outcome = 'silent'),
		(SELECT COUNT(*) FROM return_deliveries d WHERE d.ret = x.id AND d.outcome = 'failed'),
		(SELECT COUNT(*) FROM return_deliveries d WHERE d.ret = x.id AND d.truncated = 1 AND d.state = 'settled')
		FROM returns x WHERE (? = '' OR x.agent = ?) ORDER BY x.id DESC LIMIT ?`, agent, agent, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReturnRow{}
	for rows.Next() {
		var v ReturnRow
		var created, closed int64
		if err := rows.Scan(&v.ID, &v.Agent, &v.Recipient, &v.State, &v.CloseReason, &v.Request, &v.Thread, &v.Escalated, &created, &closed,
			&v.Confirmed, &v.Unconfirmed, &v.Silent, &v.Failed, &v.Truncated); err != nil {
			return nil, err
		}
		v.Created = isoMs(created)
		if closed > 0 {
			v.Closed = isoMs(closed)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
