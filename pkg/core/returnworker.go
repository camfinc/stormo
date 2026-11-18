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

// The return paths' worker: it feeds a return's events and its delivery's outcomes to the state
// machine (returns.go), carries out what it answers, and drives each delivery: a channel turn whose
// reply is posted to the origin part by part, each part journaled before it goes. It also posts the
// reply of a person's own turn the connector stopped waiting for (a delivery with no return).

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

// view is return id as its state machine sees it.
func (r *Returns) view(q querier, id int64) (ReturnView, error) {
	var v ReturnView
	var active, escalated int
	var escAt, escalatedAt, nextDue, last int64
	err := q.QueryRow(`SELECT state, prev, seq_last, seq_seen, active, attempts, depth, escalated, late_used,
		escalate_at, COALESCE((SELECT MAX(seq) FROM return_events WHERE ret = returns.id AND kind = 'answered' AND applied = 1), 0),
		escalated_at, next_due, last_activity FROM returns WHERE id = ?`, id).Scan(&v.State, &v.Prev, &v.SeqLast, &v.SeqSeen, &active,
		&v.Attempts, &v.Depth, &escalated, &v.LateUsed, &escAt, &v.AnswerSeq, &escalatedAt, &nextDue, &last)
	v.Active, v.Escalated = active != 0, escalated != 0
	v.EscalateAt, v.EscalatedAt, v.NextDue, v.LastActivity = ms(escAt), ms(escalatedAt), ms(nextDue), ms(last)
	return v, err
}

// save writes v back to return id; a closed state closes it (closed_at, once). The deliveries of its
// events that will never reach the person go back to the bus, to wake the agent as any message
// does: every one when it closed; once failed or escalated with nothing in flight, all but an
// answer the late allowance still delivers.
func (r *Returns) save(q querier, id int64, v ReturnView) error {
	if _, err := q.Exec(`UPDATE returns SET state = ?, prev = ?, seq_last = ?, seq_seen = ?, active = ?, attempts = ?, escalated = ?,
		escalated_at = ?, late_used = ?, next_due = ?, last_activity = ?, revision = revision + 1,
		closed_at = CASE WHEN ? = 1 THEN COALESCE(closed_at, ?) ELSE NULL END WHERE id = ?`,
		v.State, v.Prev, v.SeqLast, v.SeqSeen, v.Active, v.Attempts, v.Escalated, unms(v.EscalatedAt), v.LateUsed,
		unms(v.NextDue), unms(v.LastActivity), btoi(v.Terminal()), unms(r.o.Now()), id); err != nil {
		return err
	}
	if !(v.Terminal() || (v.State == ReturnFailed || v.Escalated) && !v.Active) {
		return nil
	}
	answers := v.Terminal() || v.LateUsed >= ReturnLateAnswers
	_, err := q.Exec(`UPDATE deliveries SET owner = 'bus' WHERE owner = 'return' AND agent = (SELECT agent FROM returns WHERE id = ?)
		AND message IN (SELECT message FROM return_events WHERE ret = ? AND seq > ? AND applied = 1 AND message != 0 AND (? OR kind != 'answered'))`,
		id, id, v.SeqSeen, answers)
	return err
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
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

// recover settles what a stopped core left: a part being sent may or may not have gone (unknown,
// held for a person).
func (r *Returns) recover() {
	rows, err := r.o.DB.Query(`SELECT DISTINCT delivery FROM return_parts WHERE state = 'sending'`)
	if err != nil {
		return
	}
	var ds []int64
	for rows.Next() {
		var d int64
		if rows.Scan(&d) == nil {
			ds = append(ds, d)
		}
	}
	rows.Close()
	for _, d := range ds {
		_, _ = r.o.DB.Exec(`UPDATE return_parts SET state = 'unknown' WHERE delivery = ? AND state = 'sending'`, d)
		r.outcome(d, OutcomeUnknown, "the core stopped while a part was being sent")
	}
}

// Round is one pass: the agents' negotiations refreshed, events applied, ticks, deliveries driven,
// late replies taken up, and once an hour what is old scrubbed.
func (r *Returns) Round() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, agent := range r.agents() {
		r.refresh(agent)
	}
	r.applyEvents()
	r.tick()
	r.inbounds()
	r.drive()
	if now := r.o.Now(); now.Sub(r.scrubbed) >= time.Hour {
		r.scrubbed = now
		r.scrub()
	}
}

// agents are those with a return not closed or a person's turn still to answer.
func (r *Returns) agents() []string {
	rows, err := r.o.DB.Query(`SELECT agent FROM returns WHERE state NOT IN ('done', 'expired')
		UNION SELECT agent FROM executions WHERE kind = 'inbound' AND state = 'detached'`)
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

// applyEvents feeds each return's new events, in order, to its machine. One that can no longer
// reach the person (the return closed, failed or escalated since it was recorded) goes back to
// the bus instead.
func (r *Returns) applyEvents() {
	type ev struct {
		id, ret, seq, message int64
		kind                  string
	}
	rows, err := r.o.DB.Query(`SELECT id, ret, seq, message, kind FROM return_events WHERE applied = 0 ORDER BY ret, seq`)
	if err != nil {
		return
	}
	var evs []ev
	for rows.Next() {
		var e ev
		if rows.Scan(&e.id, &e.ret, &e.seq, &e.message, &e.kind) == nil {
			evs = append(evs, e)
		}
	}
	rows.Close()
	for _, e := range evs {
		_ = r.inTx(e.ret, func(tx *sql.Tx, v ReturnView) (Step, error) {
			if _, err := tx.Exec(`UPDATE return_events SET applied = 1 WHERE id = ?`, e.id); err != nil {
				return Step{}, err
			}
			if !Eligible(v, e.kind) {
				_, err := tx.Exec(`UPDATE deliveries SET owner = 'bus' WHERE owner = 'return' AND message = ? AND agent = (SELECT agent FROM returns WHERE id = ?)`, e.message, e.ret)
				return Step{Next: v}, err
			}
			return OnEvent(v, e.kind, e.seq, r.o.Now()), nil
		})
	}
}

// tick runs the deadlines of every return not closed.
func (r *Returns) tick() {
	rows, err := r.o.DB.Query(`SELECT id FROM returns WHERE state NOT IN ('done', 'expired', 'unknown')`)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		_ = r.inTx(id, func(tx *sql.Tx, v ReturnView) (Step, error) { return OnTick(v, r.o.Now()), nil })
	}
}

// errMovedOn is a step whose delivery moved on meanwhile: nothing is saved.
var errMovedOn = errors.New("returns: the delivery moved on")

// inTx loads return id, steps its machine with f, saves it and carries out the actions, in one
// transaction: all of it, or none.
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
	if err := r.save(tx, id, step.Next); err != nil {
		log.Printf("returns: save %d: %v", id, err)
		return err
	}
	for _, a := range step.Actions {
		if err := r.act(tx, id, a); err != nil {
			log.Printf("returns: %s %d: %v", a.Kind, id, err)
			return err
		}
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
		var failures int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM return_events WHERE ret = ? AND seq BETWEEN ? AND ? AND kind = 'failure'`, id, a.From, a.To).Scan(&failures); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO return_deliveries(ret, seq_from, seq_to, event, state, failure, created, updated) VALUES(?,?,?,?,?,?,?,?)`,
			id, a.From, a.To, a.Event, "queued", failures > 0, now.UnixMilli(), now.UnixMilli())
		return err
	case ActRetry:
		// the turn failed and provably did nothing: a new execution runs it again
		_, err := tx.Exec(`UPDATE return_deliveries SET state = 'queued', execution = '', input = '', started = 0, next_try = ?, updated = ?
			WHERE ret = ? AND state IN ('queued', 'running')`, now.Add(10*time.Second).UnixMilli(), now.UnixMilli(), id)
		return err
	case ActResend:
		if _, err := tx.Exec(`UPDATE return_parts SET state = 'planned' WHERE state = 'unknown' AND delivery IN (SELECT id FROM return_deliveries WHERE ret = ? AND state = 'unknown')`, id); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE return_deliveries SET state = 'posting', attempt = 0, next_try = 0, updated = ? WHERE ret = ? AND state = 'unknown'`, now.UnixMilli(), id)
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
		raiseFindingTx(tx, agent, a.Rule, "medium", fmt.Sprintf("return %d", id), now.UnixMilli())
	case ActFallback:
		return r.fallback(tx, id, agent, a)
	}
	return nil
}

// fallback gives the events a failed delivery covered (a.From..a.To) back to the bus and leaves the
// agent a core notice in the request's thread: the bus wakes it for the notice (durably, retried
// like any message), and the events' own deliveries are marked woken so it is woken once, not
// twice. Newer events stay the return's, for its next delivery.
func (r *Returns) fallback(tx *sql.Tx, id int64, agent string, a Action) error {
	var thread, request int64
	var origin string
	if err := tx.QueryRow(`SELECT thread, COALESCE(request, 0), origin FROM returns WHERE id = ?`, id).Scan(&thread, &request, &origin); err != nil {
		return err
	}
	now := r.o.Now().UnixMilli()
	if _, err := tx.Exec(`UPDATE deliveries SET owner = 'bus', woken_at = COALESCE(woken_at, ?) WHERE agent = ? AND owner = 'return'
		AND message IN (SELECT message FROM return_events WHERE ret = ? AND seq BETWEEN ? AND ? AND message != 0)`, now, agent, id, a.From, a.To); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE return_deliveries SET state = 'fallback', updated = ? WHERE ret = ? AND state IN ('queued', 'running', 'posting')`, now, id); err != nil {
		return err
	}
	raiseFindingTx(tx, agent, "return_undelivered", "medium", fmt.Sprintf("return %d", id), now)
	reason := a.Reason
	var sent, total int
	_ = tx.QueryRow(`SELECT COUNT(CASE WHEN p.state = 'sent' THEN 1 END), COUNT(*) FROM return_parts p
		WHERE p.delivery = (SELECT MAX(id) FROM return_deliveries WHERE ret = ?)`, id).Scan(&sent, &total)
	if total > 0 {
		reason += fmt.Sprintf(" (%d of its %d parts reached them)", sent, total)
	}
	where := "their chat"
	var chat, requester string
	if tx.QueryRow(`SELECT chat, requester FROM origins WHERE id = ?`, origin).Scan(&chat, &requester) == nil {
		if number, _, _ := strings.Cut(requester, "\x00"); !strings.HasSuffix(chat, "@g.us") && number != "" {
			where = "their chat (whatsapp_send to +" + strings.TrimPrefix(number, "+") + ")"
		} else {
			where = "their group (whatsapp_send to " + chat + ")"
		}
	}
	var hop int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(hop), 1) FROM messages WHERE thread = ?`, thread).Scan(&hop); err != nil {
		return err
	}
	body := fmt.Sprintf("[RETURN FALLBACK] The update about your request %d could not reach the person automatically: %s. "+
		"Read this thread with msg_thread(%d) and tell them yourself where it stands, in %s, if you can.", request, reason, thread, where)
	res, err := tx.Exec(`INSERT INTO messages(thread, sender, audience, subject, body, priority, attach, hop, created) VALUES(?,?,?,?,?,'normal','[]',?,?)`,
		thread, CoreSender, agent, "Tell the person yourself", body, hop, now)
	if err != nil {
		return err
	}
	nid, _ := res.LastInsertId()
	_, err = tx.Exec(`INSERT INTO deliveries(message, agent) VALUES(?, ?)`, nid, agent)
	if err == nil && r.o.Bus != nil {
		r.o.Bus.Kick()
	}
	return err
}

// ---- deliveries ----------------------------------------------------------------------------

type delivery struct {
	id, ret, from, to              int64
	event, execution, input, state string
	started                        int64
	attempt                        int
	disposition, prepared          string
	failure                        bool
	nextTry                        int64
	agent, origin                  string
	request, thread                int64
	recipient                      string
}

// startPatience is how long a delivery's turn may go unconfirmed (the runtime never answering
// whether it admitted it) before it is given up as one that may have run.
const startPatience = 30 * time.Minute

// drive moves every active delivery one step. Nothing moves while the instance's switch is stop;
// a return whose agent drains (opted out, the instance draining, its engine no longer enforcing)
// gives its update back to the bus unless part of it was posted already.
func (r *Returns) drive() {
	rows, err := r.o.DB.Query(`SELECT d.id, COALESCE(d.ret, 0), d.seq_from, d.seq_to, d.event, d.execution, d.input, d.started, d.state, d.attempt,
		d.disposition, d.prepared, d.failure, d.next_try, COALESCE(x.agent, e.agent, ''), COALESCE(x.origin, e.origin, ''), COALESCE(x.request, 0),
		COALESCE(x.thread, 0), COALESCE(x.recipient, '')
		FROM return_deliveries d LEFT JOIN returns x ON x.id = d.ret LEFT JOIN executions e ON e.id = d.execution
		WHERE d.state IN ('queued', 'running', 'posting') ORDER BY d.id`)
	if err != nil {
		return
	}
	var ds []delivery
	for rows.Next() {
		var d delivery
		if rows.Scan(&d.id, &d.ret, &d.from, &d.to, &d.event, &d.execution, &d.input, &d.started, &d.state, &d.attempt, &d.disposition, &d.prepared,
			&d.failure, &d.nextTry, &d.agent, &d.origin, &d.request, &d.thread, &d.recipient) == nil {
			ds = append(ds, d)
		}
	}
	rows.Close()
	if r.o.Mode() == "stop" {
		return // held as they are, for review
	}
	now := r.o.Now().UnixMilli()
	for _, d := range ds {
		if d.nextTry > now {
			continue
		}
		drain := ""
		if d.ret != 0 {
			drain = r.draining(d.agent)
		}
		switch {
		case d.state == "queued" && drain != "":
			r.outcome(d.id, OutcomeRouteGone, drain)
		case d.state == "queued":
			r.startDelivery(d)
		case d.state == "running":
			r.pollDelivery(d, drain)
		case d.state == "posting" && drain != "" && r.sentParts(d.id) == 0:
			r.outcome(d.id, OutcomeRouteGone, drain)
		case d.state == "posting":
			r.post(d)
		}
	}
}

func (r *Returns) sentParts(delivery int64) int {
	var n int
	_ = r.o.DB.QueryRow(`SELECT COUNT(*) FROM return_parts WHERE delivery = ? AND state = 'sent'`, delivery).Scan(&n)
	return n
}

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
			case EventAcked:
				what = append(what, fmt.Sprintf("%s acknowledged it without answering", d.recipient))
			case EventFailure:
				what = append(what, "the bus says it went unanswered")
			case EventStatus:
				what = append(what, "the bus says it is still open")
			}
			if msg != 0 {
				read = append(read, fmt.Sprint(msg))
			}
		}
		rows.Close()
	}
	if d.event == EventDue || len(what) == 0 {
		what = append(what, "they have had no update for a while")
	}
	readLine := ""
	if len(read) > 0 {
		readLine = fmt.Sprintf(" Read %s with msg_read.", strings.Join(read, ", "))
	}
	if requester == "" {
		requester = "the person"
	}
	return fmt.Sprintf("[RETURN] News on your request %d to %s, which you sent for %s in this chat: %s.%s "+
		"Tell them, in this chat, what they need to know now. End your reply with one line: [DONE] if it gives them what "+
		"they asked for, [UPDATE] if it is progress, or [SILENT] if there is nothing to tell them yet.",
		d.request, d.recipient, requester, strings.Join(what, "; "), readLine)
}

// startDelivery runs a delivery's turn: a return execution in the origin's conversation. Its
// execution, token and input are kept before the runtime is asked, so an answer that never came is
// asked again with the very same request, which the runtime admits once.
func (r *Returns) startDelivery(d delivery) {
	ct, err := r.o.Channel(d.agent)
	if err != nil {
		r.outcome(d.id, OutcomeRouteGone, "the agent's channel turns are not available")
		return
	}
	now := r.o.Now()
	if d.execution == "" {
		var session string
		if r.o.DB.QueryRow(`SELECT session FROM origins WHERE id = ?`, d.origin).Scan(&session) != nil {
			r.outcome(d.id, OutcomeRouteGone, "the origin is gone")
			return
		}
		_, requester, _ := r.originOf(d.origin)
		exec, token, input := newID(), newToken(), r.continuation(d, requester)
		tx, err := r.o.DB.Begin()
		if err != nil {
			return
		}
		_, err = tx.Exec(`INSERT INTO executions(id, agent, kind, origin, token, ret, session, state, created) VALUES(?,?,?,?,?,?,?,?,?)`,
			exec, d.agent, "return", d.origin, token, d.ret, session, "running", now.UnixMilli())
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
	_, err = ct.Start(ctx, engine.ChannelTurn{Execution: d.execution, Token: token, Kind: "return", Session: session, In: engine.Input{Text: d.input}})
	switch {
	case err == nil:
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET state = 'running', error = '', updated = ? WHERE id = ? AND state = 'queued'`, now.UnixMilli(), d.id)
	case errors.Is(err, engine.ErrChannelBusy):
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET next_try = ? WHERE id = ?`, now.Add(15*time.Second).UnixMilli(), d.id)
	case errors.Is(err, engine.ErrChannelRefused):
		r.endExecution(d.execution, "failed", 0, err.Error())
		r.outcome(d.id, OutcomeTurnFailed, err.Error())
	case now.Sub(ms(d.started)) >= startPatience:
		// never confirmed either way: it may have run, so it is never run again
		r.endExecution(d.execution, "failed", 0, "never confirmed: "+err.Error())
		r.outcome(d.id, OutcomeSideEffects, "the agent's engine never confirmed the update's turn")
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
			if strings.HasSuffix(t, m) { // a marker run into the text: dropped, it decides nothing
				l = strings.TrimRight(strings.TrimSuffix(strings.TrimRight(l, " \t\r"), m), " \t")
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

// pollDelivery asks about a delivery's turn; once it ended, its reply is kept (prepared) and planned
// as parts, or its outcome taken. A turn the runtime no longer knows may have acted: never rerun.
func (r *Returns) pollDelivery(d delivery, drain string) {
	ct, err := r.o.Channel(d.agent)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := ct.Status(ctx, d.execution, 0)
	if errors.Is(err, engine.ErrChannelUnknown) {
		r.endExecution(d.execution, "failed", 0, "the engine lost the turn")
		r.outcome(d.id, OutcomeSideEffects, "the agent's engine lost the update's turn; it may have acted")
		return
	}
	if err != nil || !st.Terminal() {
		return
	}
	r.endExecution(d.execution, st.State, st.Acted, st.Error)
	switch {
	case st.State != "ok" && st.Acted == 0:
		_ = ct.Take(ctx, d.execution)
		r.outcome(d.id, OutcomeTurnFailed, st.Error)
		return
	case st.State != "ok":
		_ = ct.Take(ctx, d.execution)
		r.outcome(d.id, OutcomeSideEffects, st.Error)
		return
	case drain != "":
		_ = ct.Take(ctx, d.execution)
		r.outcome(d.id, OutcomeRouteGone, drain)
		return
	}
	disp, text := disposition(st.Text)
	if disp == DispositionSilent {
		_ = ct.Take(ctx, d.execution)
		r.outcomeWith(d.id, OutcomeSilent, DispositionSilent, "")
		return
	}
	if r.prepare(d.id, "running", disp, text, st.Marked) {
		_ = ct.Take(ctx, d.execution) // kept here now: the engine may forget it
	}
}

// prepare keeps a reply on delivery id (in state from) and plans its parts, in one transaction.
func (r *Returns) prepare(id int64, from, disp, text string, marked bool) bool {
	tx, err := r.o.DB.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()
	mark := ""
	if marked {
		mark = "marked"
	}
	now := r.o.Now().UnixMilli()
	res, err := tx.Exec(`UPDATE return_deliveries SET state = 'posting', disposition = ?, prepared = ?, prepared_mark = ?, attempt = 0, next_try = 0, updated = ?
		WHERE id = ? AND state = ?`, disp, text, mark, now, id, from)
	if n, _ := rowsOf(res); err != nil || n == 0 {
		return false
	}
	for i := range whatsapp.Parts(text) {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO return_parts(delivery, part, state, at) VALUES(?,?,?,?)`, id, i, "planned", now); err != nil {
			return false
		}
	}
	return tx.Commit() == nil
}

// errFenced is the journal refusing a part: the delivery is no longer posting, its return closed, or
// the instance's switch is stop. The part stays planned.
var errFenced = errors.New("returns: the post was stopped")

// post sends a prepared delivery's unsent parts to the origin, each journaled before it goes. A part
// is sent again only when it provably did not go; one that may have gone holds the delivery for a
// person.
func (r *Returns) post(d delivery) {
	p, err := r.o.Poster(d.agent)
	if err != nil {
		r.retryPost(d, err)
		return
	}
	o, _, err := r.originOf(d.origin)
	if err != nil {
		r.outcome(d.id, OutcomeRouteGone, "the origin is gone")
		return
	}
	var from sql.NullInt64
	var held int
	_ = r.o.DB.QueryRow(`SELECT MIN(CASE WHEN state = 'planned' THEN part END), COUNT(CASE WHEN state IN ('sending', 'unknown') THEN 1 END)
		FROM return_parts WHERE delivery = ?`, d.id).Scan(&from, &held)
	switch {
	case held > 0:
		r.outcome(d.id, OutcomeUnknown, "a part may or may not have been sent")
		return
	case !from.Valid:
		r.outcomeWith(d.id, OutcomePosted, d.disposition, "")
		return
	}
	if err := p.AllowedReply(o); err != nil {
		if errors.Is(err, whatsapp.ErrNotConnected) {
			r.retryPost(d, err)
		} else {
			r.outcome(d.id, OutcomeRouteGone, err.Error())
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err = p.PostOrigin(ctx, o, whatsapp.Parts(d.prepared), int(from.Int64), func(part int, state, id string) error {
		at := r.o.Now().UnixMilli()
		if state == "sent" {
			_, err := r.o.DB.Exec(`UPDATE return_parts SET state = 'sent', sent_id = ?, at = ? WHERE delivery = ? AND part = ?`, id, at, d.id, part)
			return err
		}
		return r.fence(d, part, at)
	})
	var pe *whatsapp.PostError
	switch {
	case err == nil:
		r.outcomeWith(d.id, OutcomePosted, d.disposition, "")
	case errors.As(err, &pe) && pe.Sent:
		// it went; recording it failed: recorded again here, or held
		if _, err := r.o.DB.Exec(`UPDATE return_parts SET state = 'sent', sent_id = ? WHERE delivery = ? AND part = ?`, pe.ID, d.id, pe.Part); err != nil {
			_, _ = r.o.DB.Exec(`UPDATE return_parts SET state = 'unknown' WHERE delivery = ? AND part = ?`, d.id, pe.Part)
			r.outcome(d.id, OutcomeUnknown, "a part was sent but could not be recorded")
		}
	case errors.As(err, &pe) && pe.Unsent && errors.Is(err, errFenced):
		// stopped before the part: the next round decides
	case errors.As(err, &pe) && pe.Unsent:
		var state string
		_ = r.o.DB.QueryRow(`SELECT state FROM return_parts WHERE delivery = ? AND part = ?`, d.id, pe.Part).Scan(&state)
		_, _ = r.o.DB.Exec(`UPDATE return_parts SET state = 'planned' WHERE delivery = ? AND part = ? AND state = 'sending'`, d.id, pe.Part)
		if state == "planned" && !errors.Is(err, whatsapp.ErrNotConnected) {
			r.outcome(d.id, OutcomeRouteGone, pe.Err.Error()) // no longer allowed to post there
			return
		}
		r.retryPost(d, err)
	default:
		// it may or may not have gone: held for a person, never sent again on its own
		_, _ = r.o.DB.Exec(`UPDATE return_parts SET state = 'unknown' WHERE delivery = ? AND state = 'sending'`, d.id)
		r.outcome(d.id, OutcomeUnknown, err.Error())
	}
}

// fence is the journal's check before part goes: the switch is not stop, the delivery still
// posting, its return not closed, and the part still planned; then it is marked sending.
func (r *Returns) fence(d delivery, part int, at int64) error {
	if r.o.Mode() == "stop" {
		return errFenced
	}
	var state string
	if r.o.DB.QueryRow(`SELECT state FROM return_deliveries WHERE id = ?`, d.id).Scan(&state) != nil || state != "posting" {
		return errFenced
	}
	if d.ret != 0 {
		var closed sql.NullInt64
		if r.o.DB.QueryRow(`SELECT closed_at FROM returns WHERE id = ?`, d.ret).Scan(&closed) != nil || closed.Valid {
			return errFenced
		}
	}
	res, err := r.o.DB.Exec(`UPDATE return_parts SET state = 'sending', at = ? WHERE delivery = ? AND part = ? AND state = 'planned'`, at, d.id, part)
	if err != nil {
		return fmt.Errorf("%w: %v", errFenced, err) // not marked: it does not go
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errFenced
	}
	return nil
}

// postTries is how often a delivery's post is tried before it falls back.
const postTries = 6

// retryPost tries a post again later (a minute more each time), or gives up.
func (r *Returns) retryPost(d delivery, cause error) {
	if d.attempt+1 < postTries {
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET attempt = attempt + 1, next_try = ?, error = ? WHERE id = ?`,
			r.o.Now().Add(time.Duration(d.attempt+1)*time.Minute).UnixMilli(), clip(cause.Error(), 300), d.id)
		return
	}
	r.outcome(d.id, OutcomePartial, cause.Error())
}

func (r *Returns) outcome(delivery int64, outcome, why string) {
	r.outcomeWith(delivery, outcome, "", why)
}

// outcomeWith closes an active delivery with its outcome and feeds it to its return's machine (a
// late reply with no return just closes, a finding when it did not reach them). One no longer
// active is left alone.
func (r *Returns) outcomeWith(delivery int64, outcome, disp, why string) {
	var ret, to int64
	var failure bool
	var dispStored, exec, agent string
	if r.o.DB.QueryRow(`SELECT COALESCE(d.ret, 0), d.seq_to, d.failure, d.disposition, d.execution, COALESCE(x.agent, e.agent, '')
		FROM return_deliveries d LEFT JOIN returns x ON x.id = d.ret LEFT JOIN executions e ON e.id = d.execution WHERE d.id = ?`, delivery).
		Scan(&ret, &to, &failure, &dispStored, &exec, &agent) != nil {
		return
	}
	if disp == "" {
		disp = dispStored
	}
	state := "done"
	switch outcome {
	case OutcomeUnknown:
		state = "unknown"
	case OutcomeTurnFailed:
		state = "" // retried (ActRetry requeues it) or fallen back
	}
	now := r.o.Now().UnixMilli()
	close := func(q querier) error {
		res, err := q.Exec(`UPDATE return_deliveries SET state = CASE WHEN ? = '' THEN state ELSE ? END, outcome = ?, error = ?, updated = ?
			WHERE id = ? AND state IN ('queued', 'running', 'posting')`, state, state, outcome, clip(why, 300), now, delivery)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errMovedOn
		}
		return nil
	}
	if ret == 0 {
		tx, err := r.o.DB.Begin()
		if err != nil {
			return
		}
		defer tx.Rollback()
		if close(tx) != nil {
			return
		}
		if outcome != OutcomePosted {
			raiseFindingTx(tx, agent, "reply_undelivered", "medium", fmt.Sprintf("a late reply (execution %s): %s", exec, outcome), now)
		}
		_ = tx.Commit()
		return
	}
	_ = r.inTx(ret, func(tx *sql.Tx, v ReturnView) (Step, error) {
		if err := close(tx); err != nil {
			return Step{}, err
		}
		return OnDelivery(v, outcome, disp, to, failure, r.o.Now()), nil
	})
}

// inbounds takes up the person's own turns the connector stopped waiting for: once one ends, its
// reply is kept and planned as a delivery with no return, posted to their chat like an update.
func (r *Returns) inbounds() {
	rows, err := r.o.DB.Query(`SELECT id, agent FROM executions WHERE kind = 'inbound' AND state = 'detached'`)
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
			r.endExecution(x.id, "failed", 0, "the engine lost the turn")
			_ = r.o.DB.RaiseFinding(x.agent, "reply_undelivered", "medium", fmt.Sprintf("a late reply (execution %s): lost", x.id), r.o.Now().UnixMilli())
		case err != nil || !st.Terminal():
		default:
			r.late(ctx, ct, x.id, x.agent, st)
		}
		cancel()
	}
}

// late keeps a detached turn's reply as a delivery (one transaction: the execution ended, the
// delivery and its parts planned), then lets the runtime forget it.
func (r *Returns) late(ctx context.Context, ct engine.ChannelTurns, exec, agent string, st engine.ChannelTurnStatus) {
	text := strings.TrimSpace(withoutSilence(st.Text))
	tx, err := r.o.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	now := r.o.Now().UnixMilli()
	res, err := tx.Exec(`UPDATE executions SET state = ?, acted = ?, error = ?, ended = ? WHERE id = ? AND state = 'detached'`, st.State, st.Acted, clip(st.Error, 300), now, exec)
	if n, _ := rowsOf(res); err != nil || n == 0 {
		return
	}
	switch {
	case st.State != "ok":
		raiseFindingTx(tx, agent, "reply_undelivered", "medium", fmt.Sprintf("a late reply (execution %s): the turn %s", exec, st.State), now)
	case text != "":
		mark := ""
		if st.Marked {
			mark = "marked"
		}
		res, err := tx.Exec(`INSERT INTO return_deliveries(ret, seq_from, seq_to, event, execution, state, disposition, prepared, prepared_mark, created, updated)
			VALUES(NULL, 0, 0, 'reply', ?, 'posting', ?, ?, ?, ?, ?)`, exec, DispositionUpdate, text, mark, now, now)
		if err != nil {
			return
		}
		id, _ := res.LastInsertId()
		for i := range whatsapp.Parts(text) {
			if _, err := tx.Exec(`INSERT INTO return_parts(delivery, part, state, at) VALUES(?,?,?,?)`, id, i, "planned", now); err != nil {
				return
			}
		}
	}
	if tx.Commit() == nil {
		_ = ct.Take(ctx, exec)
	}
}

// returnKeep is how long what a closed return said is kept: its words, the person's quote and its
// parts go after it, its states and ids stay (a thread stays bound to its person).
const returnKeep = 30 * 24 * time.Hour

// scrub clears what is old: the words of closed deliveries, the quotes and requesters of origins
// nothing open needs, and the tokens of executions ended a week ago (a call that late is refused).
func (r *Returns) scrub() {
	now := r.o.Now()
	cut := now.Add(-returnKeep).UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM return_parts WHERE delivery IN (SELECT id FROM return_deliveries WHERE updated < ? AND state IN ('done', 'superseded', 'fallback'))`, []any{cut}},
		{`UPDATE return_deliveries SET prepared = '', input = '' WHERE updated < ? AND state IN ('done', 'superseded', 'fallback') AND (prepared != '' OR input != '')`, []any{cut}},
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
	ID        int64  `json:"id"`
	Agent     string `json:"agent"`
	Recipient string `json:"recipient"`
	State     string `json:"state"`
	Request   int64  `json:"request"`
	Thread    int64  `json:"thread"`
	Escalated bool   `json:"escalated"`
	Created   string `json:"created"`
	Closed    string `json:"closed,omitempty"`
	Updates   int    `json:"updates"`  // updates posted in full
	Fallback  int    `json:"fallback"` // updates given back to the bus
	Unknown   int    `json:"unknown"`  // parts that may or may not have gone
	// Held is the parts of an update held for a person, in order (sent, unknown, planned): what
	// resolving it leaves to send.
	Held []string `json:"held,omitempty"`
}

// List is the returns, newest first (agent "" all), at most limit.
func (r *Returns) List(agent string, limit int) ([]ReturnRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.o.DB.Query(`SELECT x.id, x.agent, x.recipient, x.state, COALESCE(x.request, 0), x.thread, x.escalated, x.created, COALESCE(x.closed_at, 0),
		(SELECT COUNT(*) FROM return_deliveries d WHERE d.ret = x.id AND d.outcome = 'posted'),
		(SELECT COUNT(*) FROM return_deliveries d WHERE d.ret = x.id AND d.state = 'fallback' OR d.ret = x.id AND d.outcome IN ('partial', 'side_effects', 'route_gone')),
		(SELECT COUNT(*) FROM return_parts p JOIN return_deliveries d ON d.id = p.delivery WHERE d.ret = x.id AND p.state = 'unknown'),
		COALESCE((SELECT group_concat(p.state, ',') FROM (SELECT p.state FROM return_parts p JOIN return_deliveries d ON d.id = p.delivery
			WHERE d.ret = x.id AND d.state = 'unknown' ORDER BY p.part) p), '')
		FROM returns x WHERE (? = '' OR x.agent = ?) ORDER BY x.id DESC LIMIT ?`, agent, agent, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReturnRow{}
	for rows.Next() {
		var v ReturnRow
		var created, closed int64
		var held string
		if err := rows.Scan(&v.ID, &v.Agent, &v.Recipient, &v.State, &v.Request, &v.Thread, &v.Escalated, &created, &closed, &v.Updates, &v.Fallback, &v.Unknown, &held); err != nil {
			return nil, err
		}
		v.Created = isoMs(created)
		if closed > 0 {
			v.Closed = isoMs(closed)
		}
		if held != "" {
			v.Held = strings.Split(held, ",")
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Resolve is a person settling a held (unknown) update: sent (its unknown part reached them) or
// resend (it did not: it goes again). Posting resumes from the first part not sent; the update's
// own outcome applies only once every part is sent.
func (r *Returns) Resolve(id int64, sent bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var d, to int64
	var failure bool
	var disp string
	if err := r.o.DB.QueryRow(`SELECT id, seq_to, failure, disposition FROM return_deliveries WHERE ret = ? AND state = 'unknown' ORDER BY id DESC LIMIT 1`, id).
		Scan(&d, &to, &failure, &disp); err != nil {
		return toolErr("return %d has no update held as unknown", id)
	}
	err := r.inTx(id, func(tx *sql.Tx, v ReturnView) (Step, error) {
		state := "planned"
		if sent {
			state = "sent"
		}
		if _, err := tx.Exec(`UPDATE return_parts SET state = ? WHERE delivery = ? AND state = 'unknown'`, state, d); err != nil {
			return Step{}, err
		}
		var left int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM return_parts WHERE delivery = ? AND state != 'sent'`, d).Scan(&left); err != nil {
			return Step{}, err
		}
		if left == 0 {
			if _, err := tx.Exec(`UPDATE return_deliveries SET state = 'done', outcome = 'posted', updated = ? WHERE id = ?`, r.o.Now().UnixMilli(), d); err != nil {
				return Step{}, err
			}
		}
		return OnResolve(v, left == 0, disp, to, failure, r.o.Now()), nil
	})
	if err != nil {
		return err
	}
	r.Kick()
	return nil
}
