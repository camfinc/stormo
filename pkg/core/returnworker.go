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
// reply is posted to the origin part by part, each part journaled before it goes.

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
	err := q.QueryRow(`SELECT state, prev, seq_last, seq_seen, active, attempts, depth, children, escalated, late_used,
		escalate_at, COALESCE((SELECT MAX(seq) FROM return_events WHERE ret = returns.id AND kind = 'answered' AND child = 0 AND applied = 1), 0),
		escalated_at, next_due, last_activity FROM returns WHERE id = ?`, id).Scan(&v.State, &v.Prev, &v.SeqLast, &v.SeqSeen, &active,
		&v.Attempts, &v.Depth, &v.Children, &escalated, &v.LateUsed, &escAt, &v.AnswerSeq, &escalatedAt, &nextDue, &last)
	v.Active, v.Escalated = active != 0, escalated != 0
	v.EscalateAt, v.EscalatedAt, v.NextDue, v.LastActivity = ms(escAt), ms(escalatedAt), ms(nextDue), ms(last)
	return v, err
}

// save writes v back to return id; a closed state closes it (closed_at, once).
func (r *Returns) save(q querier, id int64, v ReturnView) error {
	_, err := q.Exec(`UPDATE returns SET state = ?, prev = ?, seq_last = ?, seq_seen = ?, active = ?, attempts = ?, children = ?, escalated = ?,
		escalated_at = ?, late_used = ?, next_due = ?, last_activity = ?, revision = revision + 1,
		closed_at = CASE WHEN ? = 1 THEN COALESCE(closed_at, ?) ELSE NULL END WHERE id = ?`,
		v.State, v.Prev, v.SeqLast, v.SeqSeen, v.Active, v.Attempts, v.Children, v.Escalated, unms(v.EscalatedAt), v.LateUsed,
		unms(v.NextDue), unms(v.LastActivity), btoi(v.Terminal()), unms(r.o.Now()), id)
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
// held for a person), a turn a delivery started is asked about again.
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

// Round is one pass: the agents' negotiations refreshed, events applied, ticks, deliveries driven.
func (r *Returns) Round() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, agent := range r.agents() {
		r.refresh(agent)
	}
	r.applyEvents()
	r.tick()
	r.drive()
}

// agents are those with a return not closed.
func (r *Returns) agents() []string {
	rows, err := r.o.DB.Query(`SELECT DISTINCT agent FROM returns WHERE state NOT IN ('done', 'expired')`)
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

// applyEvents feeds each return's new events, in order, to its machine.
func (r *Returns) applyEvents() {
	type ev struct {
		id, ret, seq int64
		kind         string
		child        bool
	}
	rows, err := r.o.DB.Query(`SELECT id, ret, seq, kind, child FROM return_events WHERE applied = 0 ORDER BY ret, seq`)
	if err != nil {
		return
	}
	var evs []ev
	for rows.Next() {
		var e ev
		if rows.Scan(&e.id, &e.ret, &e.seq, &e.kind, &e.child) == nil {
			evs = append(evs, e)
		}
	}
	rows.Close()
	for _, e := range evs {
		r.inTx(e.ret, func(tx *sql.Tx, v ReturnView) (Step, error) {
			if _, err := tx.Exec(`UPDATE return_events SET applied = 1 WHERE id = ?`, e.id); err != nil {
				return Step{}, err
			}
			return OnEvent(v, e.kind, e.seq, e.child, r.o.Now()), nil
		})
	}
}

// tick runs the deadlines of every return not closed.
func (r *Returns) tick() {
	rows, err := r.o.DB.Query(`SELECT id FROM returns WHERE state NOT IN ('done', 'expired', 'unknown') AND parent IS NULL`)
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
		r.inTx(id, func(tx *sql.Tx, v ReturnView) (Step, error) { return OnTick(v, r.o.Now()), nil })
	}
}

// inTx loads return id, steps its machine with f, saves it and carries out the actions, in one
// transaction (the actions that wake or post run after it).
func (r *Returns) inTx(id int64, f func(*sql.Tx, ReturnView) (Step, error)) {
	tx, err := r.o.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	v, err := r.view(tx, id)
	if err != nil {
		return
	}
	step, err := f(tx, v)
	if err != nil {
		return
	}
	if err := r.save(tx, id, step.Next); err != nil {
		log.Printf("returns: save %d: %v", id, err)
		return
	}
	var after []func()
	for _, a := range step.Actions {
		if fn := r.act(tx, id, step.Next, a); fn != nil {
			after = append(after, fn)
		}
	}
	if err := tx.Commit(); err != nil {
		return
	}
	for _, fn := range after {
		fn()
	}
}

// act carries out an action inside the transaction; what must run after it (a wake) is returned.
func (r *Returns) act(tx *sql.Tx, id int64, v ReturnView, a Action) func() {
	now := r.o.Now()
	switch a.Kind {
	case ActEnqueue:
		failure := 0
		var n int
		_ = tx.QueryRow(`SELECT COUNT(*) FROM return_events WHERE ret = ? AND seq BETWEEN ? AND ? AND kind = 'failure' AND child = 0`, id, a.From, a.To).Scan(&n)
		if n > 0 {
			failure = 1
		}
		if _, err := tx.Exec(`INSERT INTO return_deliveries(ret, seq_from, seq_to, event, state, failure, created, updated) VALUES(?,?,?,?,?,?,?,?)`,
			id, a.From, a.To, a.Event, "queued", failure, now.UnixMilli(), now.UnixMilli()); err != nil {
			log.Printf("returns: enqueue %d: %v", id, err)
		}
	case ActRetry:
		_, _ = tx.Exec(`UPDATE return_deliveries SET state = 'queued', execution = '', attempt = attempt + 1, next_try = ?, updated = ?
			WHERE ret = ? AND state NOT IN ('done', 'superseded', 'fallback', 'unknown')`, now.Add(10*time.Second).UnixMilli(), now.UnixMilli(), id)
	case ActResend:
		_, _ = tx.Exec(`UPDATE return_parts SET state = 'planned' WHERE state = 'unknown' AND delivery IN (SELECT id FROM return_deliveries WHERE ret = ? AND state = 'unknown')`, id)
		_, _ = tx.Exec(`UPDATE return_deliveries SET state = 'posting', updated = ? WHERE ret = ? AND state = 'unknown'`, now.UnixMilli(), id)
	case ActSchedule:
		var agent string
		if tx.QueryRow(`SELECT agent FROM returns WHERE id = ?`, id).Scan(&agent) == nil {
			if set := r.o.Settings(agent); set != nil {
				sched, loc := set.Schedule()
				_, _ = tx.Exec(`UPDATE returns SET next_due = ? WHERE id = ?`, sched.AddWorking(now, set.UpdateAfter, loc).UnixMilli(), id)
			}
		}
	case ActEscalate:
		var agent string
		_ = tx.QueryRow(`SELECT agent FROM returns WHERE id = ?`, id).Scan(&agent)
		raiseFindingTx(tx, agent, "return_escalated", "high", fmt.Sprintf("return %d", id), now.UnixMilli())
	case ActFinding:
		var agent string
		_ = tx.QueryRow(`SELECT agent FROM returns WHERE id = ?`, id).Scan(&agent)
		raiseFindingTx(tx, agent, a.Rule, "medium", fmt.Sprintf("return %d", id), now.UnixMilli())
	case ActFallback:
		return r.fallback(tx, id, a.Reason)
	}
	return nil
}

// fallback gives a return's undelivered update back to the bus: its event deliveries become the
// bus's (marked woken, so the ordinary wake does not take them too) and the agent is woken in the
// bus thread, once, to tell the person itself.
func (r *Returns) fallback(tx *sql.Tx, id int64, reason string) func() {
	var agent string
	var thread, request int64
	if tx.QueryRow(`SELECT agent, thread, COALESCE(request, 0) FROM returns WHERE id = ?`, id).Scan(&agent, &thread, &request) != nil {
		return nil
	}
	now := r.o.Now().UnixMilli()
	_, _ = tx.Exec(`UPDATE deliveries SET owner = 'bus', woken_at = COALESCE(woken_at, ?), session = ? WHERE agent = ? AND owner = 'return'
		AND message IN (SELECT message FROM return_events WHERE ret = ? AND message != 0)`, now, fmt.Sprintf("swarm-bus-%d", thread), agent, id)
	_, _ = tx.Exec(`UPDATE return_deliveries SET state = 'fallback', updated = ? WHERE ret = ? AND state NOT IN ('done', 'superseded', 'fallback', 'unknown')`, now, id)
	raiseFindingTx(tx, agent, "return_undelivered", "medium", fmt.Sprintf("return %d", id), now)
	_, _ = tx.Exec(`INSERT INTO wakes(agent, at) VALUES(?, ?)`, agent, now)
	input := fmt.Sprintf("[RETURN FALLBACK] The update about your request %d could not reach the person automatically: %s. "+
		"Read the thread with msg_thread(%d) and tell them yourself where it stands, in their chat, if you can.", request, reason, thread)
	return func() {
		if r.o.Bus == nil || r.o.Bus.o.Wake == nil {
			return
		}
		a, ok := r.o.Bus.roster()[agent]
		if !ok || !a.Running || a.Endpoint == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := r.o.Bus.o.Wake(ctx, a, input, fmt.Sprintf("swarm-bus-%d", thread), fmt.Sprintf("swarm-return-%d-fallback", id)); err != nil {
			log.Printf("returns: fallback wake %s: %v", agent, err)
		}
	}
}

// ---- deliveries ----------------------------------------------------------------------------

type delivery struct {
	id, ret, from, to       int64
	event, execution, state string
	attempt                 int
	disposition, prepared   string
	failure                 bool
	nextTry                 int64
	agent, origin           string
	request, thread         int64
	recipient               string
}

// drive moves every active delivery one step.
func (r *Returns) drive() {
	rows, err := r.o.DB.Query(`SELECT d.id, d.ret, d.seq_from, d.seq_to, d.event, d.execution, d.state, d.attempt, d.disposition, d.prepared, d.failure,
		d.next_try, x.agent, x.origin, COALESCE(x.request, 0), x.thread, x.recipient
		FROM return_deliveries d JOIN returns x ON x.id = d.ret WHERE d.state IN ('queued', 'running', 'posting') ORDER BY d.id`)
	if err != nil {
		return
	}
	var ds []delivery
	for rows.Next() {
		var d delivery
		if rows.Scan(&d.id, &d.ret, &d.from, &d.to, &d.event, &d.execution, &d.state, &d.attempt, &d.disposition, &d.prepared, &d.failure,
			&d.nextTry, &d.agent, &d.origin, &d.request, &d.thread, &d.recipient) == nil {
			ds = append(ds, d)
		}
	}
	rows.Close()
	now := r.o.Now().UnixMilli()
	for _, d := range ds {
		if d.nextTry > now {
			continue
		}
		switch {
		case r.o.Mode() == "stop":
			// nothing more is posted: held as it is for review
		case d.state == "queued" && r.o.Mode() == "drain":
			r.outcome(d.id, OutcomeRouteGone, "the return paths are draining")
		case d.state == "queued":
			r.startDelivery(d)
		case d.state == "running":
			r.pollDelivery(d)
		case d.state == "posting":
			r.post(d)
		}
	}
}

// continuation is the id-only input of a delivery's turn: what happened, where to read it, never
// the reply's words (reading them with msg_read marks the run as any read does).
func (r *Returns) continuation(d delivery, requester string) string {
	rows, err := r.o.DB.Query(`SELECT kind, message, child FROM return_events WHERE ret = ? AND seq BETWEEN ? AND ? ORDER BY seq`, d.ret, d.from, d.to)
	var what, read []string
	if err == nil {
		for rows.Next() {
			var kind string
			var msg int64
			var child bool
			if rows.Scan(&kind, &msg, &child) != nil {
				continue
			}
			who := d.recipient
			if child {
				who = "the agent you asked on their behalf"
			}
			switch kind {
			case EventAnswered:
				what = append(what, fmt.Sprintf("%s answered", who))
			case EventProgress:
				what = append(what, fmt.Sprintf("%s wrote, without marking it the answer", who))
			case EventAcked:
				what = append(what, fmt.Sprintf("%s acknowledged it without answering", who))
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

// startDelivery runs a delivery's turn: a return execution in the origin's conversation.
func (r *Returns) startDelivery(d delivery) {
	ct, err := r.o.Channel(d.agent)
	if err != nil {
		r.outcome(d.id, OutcomeRouteGone, "the agent's channel turns are not available")
		return
	}
	var session string
	if r.o.DB.QueryRow(`SELECT session FROM origins WHERE id = ?`, d.origin).Scan(&session) != nil {
		r.outcome(d.id, OutcomeRouteGone, "the origin is gone")
		return
	}
	_, requester, _ := r.originOf(d.origin)
	exec := newID()
	if _, err := r.o.DB.Exec(`INSERT INTO executions(id, agent, kind, origin, ret, session, state, created) VALUES(?,?,?,?,?,?,?,?)`,
		exec, d.agent, "return", d.origin, d.ret, session, "running", r.o.Now().UnixMilli()); err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = ct.Start(ctx, engine.ChannelTurn{Execution: exec, Kind: "return", Session: session, In: engine.Input{Text: r.continuation(d, requester)}})
	switch {
	case errors.Is(err, engine.ErrChannelBusy):
		r.endExecution(exec, "failed", 0, "busy")
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET next_try = ? WHERE id = ?`, r.o.Now().Add(15*time.Second).UnixMilli(), d.id)
	case err != nil:
		r.endExecution(exec, "failed", 0, err.Error())
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET next_try = ?, error = ? WHERE id = ?`, r.o.Now().Add(30*time.Second).UnixMilli(), clip(err.Error(), 300), d.id)
	default:
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET state = 'running', execution = ?, updated = ? WHERE id = ? AND state = 'queued'`, exec, r.o.Now().UnixMilli(), d.id)
	}
}

// disposition reads a reply's last line: [DONE], [UPDATE] or [SILENT] (absent or several: update),
// and the reply without it.
func disposition(text string) (string, string) {
	t := strings.TrimRight(text, " \t\r\n")
	for marker, disp := range map[string]string{"[DONE]": DispositionDone, "[UPDATE]": DispositionUpdate, "[SILENT]": DispositionSilent} {
		if strings.HasSuffix(t, marker) {
			rest := strings.TrimRight(strings.TrimSuffix(t, marker), " \t\r\n")
			if strings.TrimSpace(rest) == "" {
				return DispositionSilent, ""
			}
			if disp == DispositionSilent {
				return DispositionSilent, ""
			}
			return disp, rest
		}
	}
	if strings.TrimSpace(t) == "" {
		return DispositionSilent, ""
	}
	return DispositionUpdate, t
}

// pollDelivery asks about a delivery's turn; once it ended, its reply is kept (prepared) and planned
// as parts, or its outcome taken.
func (r *Returns) pollDelivery(d delivery) {
	ct, err := r.o.Channel(d.agent)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := ct.Status(ctx, d.execution, 0)
	if errors.Is(err, engine.ErrChannelUnknown) {
		r.outcome(d.id, OutcomeTurnFailed, "the engine does not know the turn")
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
	}
	disp, text := disposition(st.Text)
	if disp == DispositionSilent {
		_ = ct.Take(ctx, d.execution)
		r.outcomeWith(d.id, OutcomeSilent, DispositionSilent, "")
		return
	}
	parts := whatsapp.Parts(text)
	tx, err := r.o.DB.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	mark := ""
	if st.Marked {
		mark = "marked"
	}
	if _, err := tx.Exec(`UPDATE return_deliveries SET state = 'posting', disposition = ?, prepared = ?, prepared_mark = ?, updated = ? WHERE id = ? AND state = 'running'`,
		disp, text, mark, r.o.Now().UnixMilli(), d.id); err != nil {
		return
	}
	for i := range parts {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO return_parts(delivery, part, state, at) VALUES(?,?,?,?)`, d.id, i, "planned", r.o.Now().UnixMilli()); err != nil {
			return
		}
	}
	if tx.Commit() == nil {
		_ = ct.Take(ctx, d.execution) // kept here now: the engine may forget it
	}
}

// post sends a prepared delivery's unsent parts to the origin, each journaled before it goes.
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
	if err := p.AllowedReply(o); err != nil {
		if errors.Is(err, whatsapp.ErrNotConnected) {
			r.retryPost(d, err)
			return
		}
		r.outcome(d.id, OutcomeRouteGone, err.Error())
		return
	}
	parts := whatsapp.Parts(d.prepared)
	from := 0
	var sent int
	_ = r.o.DB.QueryRow(`SELECT COUNT(*) FROM return_parts WHERE delivery = ? AND state = 'sent'`, d.id).Scan(&sent)
	from = sent
	if from >= len(parts) {
		r.outcomeWith(d.id, OutcomePosted, d.disposition, "")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	err = p.PostOrigin(ctx, o, parts, from, func(part int, state, id string) error {
		_, err := r.o.DB.Exec(`UPDATE return_parts SET state = ?, sent_id = ?, at = ? WHERE delivery = ? AND part = ?`, state, id, r.o.Now().UnixMilli(), d.id, part)
		return err
	})
	switch {
	case err == nil:
		r.outcomeWith(d.id, OutcomePosted, d.disposition, "")
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		// a part may or may not have gone: held for a person
		_, _ = r.o.DB.Exec(`UPDATE return_parts SET state = 'unknown' WHERE delivery = ? AND state = 'sending'`, d.id)
		r.outcome(d.id, OutcomeUnknown, err.Error())
	default:
		// the part being sent did not go (the send answered an error): it goes again, or falls back
		_, _ = r.o.DB.Exec(`UPDATE return_parts SET state = 'planned' WHERE delivery = ? AND state = 'sending'`, d.id)
		r.retryPost(d, err)
	}
}

// postTries is how often a delivery's post is tried before it falls back.
const postTries = 5

// retryPost tries a post again later, or gives up: partial (some parts reached them) or nothing.
func (r *Returns) retryPost(d delivery, cause error) {
	if d.attempt+1 < postTries {
		_, _ = r.o.DB.Exec(`UPDATE return_deliveries SET attempt = attempt + 1, next_try = ?, error = ? WHERE id = ?`,
			r.o.Now().Add(time.Duration(d.attempt+1)*30*time.Second).UnixMilli(), clip(cause.Error(), 300), d.id)
		return
	}
	r.outcome(d.id, OutcomePartial, cause.Error())
}

func (r *Returns) outcome(delivery int64, outcome, why string) {
	r.outcomeWith(delivery, outcome, "", why)
}

// outcomeWith feeds a delivery's outcome to its return's machine and closes the delivery.
func (r *Returns) outcomeWith(delivery int64, outcome, disp, why string) {
	var ret, to int64
	var failure bool
	var dispStored string
	if r.o.DB.QueryRow(`SELECT ret, seq_to, failure, disposition FROM return_deliveries WHERE id = ?`, delivery).Scan(&ret, &to, &failure, &dispStored) != nil {
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
	r.inTx(ret, func(tx *sql.Tx, v ReturnView) (Step, error) {
		if state != "" {
			if _, err := tx.Exec(`UPDATE return_deliveries SET state = ?, error = ?, updated = ? WHERE id = ?`, state, clip(why, 300), r.o.Now().UnixMilli(), delivery); err != nil {
				return Step{}, err
			}
		}
		if outcome == OutcomeTurnFailed {
			_, _ = tx.Exec(`UPDATE return_deliveries SET error = ? WHERE id = ?`, clip(why, 300), delivery)
		}
		return OnDelivery(v, outcome, disp, to, failure, r.o.Now()), nil
	})
}
