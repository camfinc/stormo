package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/shared"
)

// The agent message bus (docs/core.md §5, phase 5): agent-to-agent work that should not clutter
// Slack. Agents send and read through MCP tools (msg_*); the core stores messages in core.db and
// wakes an idle recipient with a run on its own API, so agent A can ask agent B and get an answer
// with no person in the loop. Loop guards keep two agents from talking forever: a per-pair rate,
// a hop limit and a lifetime per thread, and a fleet-wide cap on wake runs. A breach is refused
// and becomes a finding. Message bodies stay in core.db; only the owner token or a party to the
// message reads them.

// BusAgent is one agent as the bus sees it.
type BusAgent struct {
	ID       string
	Unit     string
	Engine   string // engine.kind: how to wake it
	Running  bool
	Busy     bool // a turn, a job or a tool call is running
	Endpoint string
}

// Wake starts a turn on the agent's API with input, in session (engine.Runtime.WakeRequest).
type WakeFn func(ctx context.Context, a BusAgent, input, session, idempotency string) error

// MirrorFn posts text to the agent's own Slack home channel (urgent messages).
type MirrorFn func(agent, text string) error

// BusLimits are the loop guards; zero values take the defaults.
type BusLimits struct {
	PairPerHour  int           // messages one agent may send another in an hour (20)
	MaxHops      int           // messages in one thread (12)
	ThreadTTL    time.Duration // a thread takes no new message after this (24 h)
	WakesPerHour int           // wake runs across the fleet in an hour (30)
	WakeTries    int           // failed wake attempts before a delivery waits for msg_inbox (3)
}

// BusOptions configure a Bus.
type BusOptions struct {
	DB     *DB
	Roster func() []BusAgent
	Wake   WakeFn
	Mirror MirrorFn // nil: urgent messages are not mirrored
	Limits BusLimits
	Now    func() time.Time
}

// Bus is the message bus; safe for concurrent use.
type Bus struct {
	o    BusOptions
	kick chan struct{}
	mu   sync.Mutex // one wake round at a time
}

// RetainMessages is how long a thread is kept after its last message (open decision 6).
const RetainMessages = 90 * 24 * time.Hour

const (
	maxSubject = 200
	maxBody    = 16 << 10
	maxAttach  = 10
)

// NewBus builds a bus; call Run for the wake loop.
func NewBus(o BusOptions) *Bus {
	l := &o.Limits
	if l.PairPerHour == 0 {
		l.PairPerHour = 20
	}
	if l.MaxHops == 0 {
		l.MaxHops = 12
	}
	if l.ThreadTTL == 0 {
		l.ThreadTTL = 24 * time.Hour
	}
	if l.WakesPerHour == 0 {
		l.WakesPerHour = 30
	}
	if l.WakeTries == 0 {
		l.WakeTries = 3
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Bus{o: o, kick: make(chan struct{}, 1)}
}

// Message is one message as a party to it sees it.
type Message struct {
	ID       int64    `json:"id"`
	Thread   int64    `json:"thread"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Subject  string   `json:"subject"`
	Body     string   `json:"body,omitempty"`
	Priority string   `json:"priority"`
	Attach   []string `json:"attach"`
	Hop      int      `json:"hop"`
	Sent     string   `json:"sent"`
	// The caller's delivery: when it read and acknowledged the message (recipients only).
	Read  string `json:"read,omitempty"`
	Acked string `json:"acked,omitempty"`
	// Recipients and their state (the sender's view and the owner's).
	Recipients []Recipient `json:"recipients,omitempty"`
}

// Recipient is one delivery of a message.
type Recipient struct {
	Agent string `json:"agent"`
	Read  string `json:"read,omitempty"`
	Acked string `json:"acked,omitempty"`
	Woken string `json:"woken,omitempty"`
}

// SendRequest is msg_send's arguments.
type SendRequest struct {
	To       string   `json:"to"`
	Subject  string   `json:"subject"`
	Body     string   `json:"body"`
	Thread   int64    `json:"thread,omitempty"`
	Priority string   `json:"priority,omitempty"`
	Attach   []string `json:"attach,omitempty"`
}

// SendResult is what msg_send answers.
type SendResult struct {
	ID         int64    `json:"id"`
	Thread     int64    `json:"thread"`
	Recipients []string `json:"recipients"`
	// Recipients that are not running: they get it when they next read their inbox.
	Offline []string `json:"offline,omitempty"`
}

func (b *Bus) roster() map[string]BusAgent {
	out := map[string]BusAgent{}
	if b.o.Roster != nil {
		for _, a := range b.o.Roster() {
			out[a.ID] = a
		}
	}
	return out
}

// refusal is a loop-guard or boundary breach: the sender reads Msg, the owner gets a finding.
type refusal struct {
	ToolError
	agent, rule, evidence string
}

func (b *Bus) refuse(agent, rule, evidence string, format string, a ...any) error {
	return refusal{toolErr(format, a...).(ToolError), agent, rule, evidence}
}

// attachPath checks one attachment: a path in the shared space the sender can see, which every
// recipient can see too (a unit's private layer never goes to another unit).
func attachPath(p string, sender BusAgent, recipients []BusAgent) (string, error) {
	if strings.Contains(p, "\x00") || !strings.HasPrefix(p, "/") {
		return "", toolErr("attachment %q: give the absolute path in the shared space (/shared/group/… or /shared/%s/…)", p, sender.Unit)
	}
	clean := path.Clean(p)
	parts := strings.SplitN(strings.TrimPrefix(clean, "/"), "/", 3)
	if len(parts) < 3 || "/"+parts[0] != shared.Root || parts[2] == "" {
		return "", toolErr("attachment %q: only files in the shared space (/shared/group/… or /shared/%s/…) can be attached; send the path, never the contents", p, sender.Unit)
	}
	layer := parts[1]
	if layer != "group" && layer != sender.Unit {
		return "", toolErr("attachment %q: that layer is not yours", p)
	}
	if layer != "group" {
		for _, r := range recipients {
			if r.Unit != layer {
				return "", ToolError{fmt.Sprintf("attachment %q is private to unit %s and %s is in %s: copy what may be shared into /shared/group first (no client personal data there)", p, layer, r.ID, r.Unit)}
			}
		}
	}
	return clean, nil
}

// Send stores a message from sender and schedules its wake runs. A refused message raises a
// finding, written once the send's transaction is over (core.db has one connection).
func (b *Bus) Send(sender string, req SendRequest) (*SendResult, error) {
	out, err := b.send(sender, req)
	var r refusal
	if errors.As(err, &r) {
		if ferr := b.o.DB.RaiseFinding(r.agent, r.rule, "medium", r.evidence, b.o.Now().UnixMilli()); ferr != nil {
			log.Printf("bus: finding %s: %v", r.rule, ferr)
		}
		return nil, r.ToolError
	}
	return out, err
}

func (b *Bus) send(sender string, req SendRequest) (*SendResult, error) {
	now := b.o.Now()
	req.Subject = strings.TrimSpace(req.Subject)
	if req.Subject == "" || len([]rune(req.Subject)) > maxSubject {
		return nil, toolErr("subject is required and at most %d characters", maxSubject)
	}
	if strings.TrimSpace(req.Body) == "" || len(req.Body) > maxBody {
		return nil, toolErr("body is required and at most %d bytes; put long material in the shared space and attach its path", maxBody)
	}
	if req.Priority == "" {
		req.Priority = "normal"
	}
	if req.Priority != "normal" && req.Priority != "urgent" {
		return nil, toolErr(`priority is "normal" or "urgent"`)
	}
	if len(req.Attach) > maxAttach {
		return nil, toolErr("at most %d attachments", maxAttach)
	}
	fleet := b.roster()
	from, ok := fleet[sender]
	if !ok {
		return nil, toolErr("the core does not know agent %s", sender)
	}
	to := strings.TrimSpace(req.To)
	var recipients []BusAgent
	switch {
	case to == "group":
		for _, a := range fleet {
			recipients = append(recipients, a)
		}
	case strings.HasPrefix(to, "unit:"):
		unit := strings.TrimPrefix(to, "unit:")
		for _, a := range fleet {
			if a.Unit == unit {
				recipients = append(recipients, a)
			}
		}
		if len(recipients) == 0 {
			return nil, toolErr("no agent in unit %q", unit)
		}
	default:
		a, ok := fleet[to]
		if !ok {
			ids := []string{}
			for id := range fleet {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			return nil, toolErr("unknown recipient %q: an agent (%s), unit:<id> or group", to, strings.Join(ids, ", "))
		}
		recipients = []BusAgent{a}
	}
	recipients = slices.DeleteFunc(recipients, func(a BusAgent) bool { return a.ID == sender })
	if len(recipients) == 0 {
		return nil, toolErr("no one to send to (a message to yourself is a note: keep it in your memory)")
	}
	sort.Slice(recipients, func(i, j int) bool { return recipients[i].ID < recipients[j].ID })
	attach := []string{}
	for _, p := range req.Attach {
		clean, err := attachPath(p, from, recipients)
		if err != nil {
			if strings.Contains(err.Error(), "is private to unit") {
				return nil, b.refuse(sender, "unit_boundary", "attachment "+p+" to "+to, "%s", err.Error())
			}
			return nil, err
		}
		attach = append(attach, clean)
	}

	tx, err := b.o.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	thread, hop := int64(0), 1
	if req.Thread != 0 {
		var root int64
		var created int64
		err := tx.QueryRow(`SELECT m.thread, r.created FROM messages m JOIN messages r ON r.id = m.thread WHERE m.id = ?`, req.Thread).Scan(&root, &created)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, toolErr("no message %d to reply to", req.Thread)
		}
		if err != nil {
			return nil, err
		}
		var party int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM messages m LEFT JOIN deliveries d ON d.message = m.id AND d.agent = ? WHERE m.thread = ? AND (m.sender = ? OR d.agent IS NOT NULL)`,
			sender, root, sender).Scan(&party); err != nil {
			return nil, err
		}
		if party == 0 {
			return nil, toolErr("thread %d is not one you are part of", req.Thread)
		}
		if now.Sub(time.UnixMilli(created)) > b.o.Limits.ThreadTTL {
			return nil, b.refuse(sender, "bus_thread_ttl", fmt.Sprintf("thread %d", root),
				"thread %d is older than %s and takes no more messages: start a new one if the work is really still open", root, b.o.Limits.ThreadTTL)
		}
		if err := tx.QueryRow(`SELECT COALESCE(MAX(hop), 0) + 1 FROM messages WHERE thread = ?`, root).Scan(&hop); err != nil {
			return nil, err
		}
		if hop > b.o.Limits.MaxHops {
			return nil, b.refuse(sender, "bus_hops", fmt.Sprintf("thread %d", root),
				"thread %d already has %d messages: the conversation is looping. Stop and tell a person in Slack what is unresolved", root, b.o.Limits.MaxHops)
		}
		thread = root
	}
	hour := now.Add(-time.Hour).UnixMilli()
	for _, r := range recipients {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM messages m JOIN deliveries d ON d.message = m.id WHERE m.sender = ? AND d.agent = ? AND m.created >= ?`,
			sender, r.ID, hour).Scan(&n); err != nil {
			return nil, err
		}
		if n >= b.o.Limits.PairPerHour {
			return nil, b.refuse(sender, "bus_rate", "to "+r.ID,
				"you sent %s %d messages in the last hour, the limit: wait, or batch what you need into one message", r.ID, n)
		}
	}
	att, _ := json.Marshal(attach)
	res, err := tx.Exec(`INSERT INTO messages(thread, sender, audience, subject, body, priority, attach, hop, created) VALUES(?,?,?,?,?,?,?,?,?)`,
		thread, sender, to, req.Subject, req.Body, req.Priority, string(att), hop, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if thread == 0 {
		thread = id
		if _, err := tx.Exec(`UPDATE messages SET thread = ? WHERE id = ?`, id, id); err != nil {
			return nil, err
		}
	}
	out := &SendResult{ID: id, Thread: thread, Recipients: []string{}}
	for _, r := range recipients {
		if _, err := tx.Exec(`INSERT INTO deliveries(message, agent) VALUES(?, ?)`, id, r.ID); err != nil {
			return nil, err
		}
		out.Recipients = append(out.Recipients, r.ID)
		if !r.Running {
			out.Offline = append(out.Offline, r.ID)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if req.Priority == "urgent" && b.o.Mirror != nil {
		for _, r := range recipients {
			text := fmt.Sprintf("Urgent message from %s on the swarm bus: %s (I'll read it with msg_inbox, message %d).", sender, req.Subject, id)
			if err := b.o.Mirror(r.ID, text); err != nil {
				log.Printf("bus: urgent mirror to %s: %v", r.ID, err)
			}
		}
	}
	b.Kick()
	return out, nil
}

// Kick asks the wake loop for a round now.
func (b *Bus) Kick() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Run is the wake loop: a round every interval and on every Kick, until stop closes.
func (b *Bus) Run(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		case <-b.kick:
		}
		b.WakeRound()
	}
}

// wakeInput is the turn a woken agent starts with. It names the tools, never the content.
func wakeInput(from []string, subject string, n int, thread int64) string {
	more := ""
	if n > 1 {
		more = fmt.Sprintf(" (and %d more)", n-1)
	}
	return fmt.Sprintf("[AGENT MESSAGE from %s] %s%s. Read your swarm inbox with msg_inbox and msg_read. "+
		"If it needs an answer, reply with msg_send(thread=%d); acknowledge with msg_ack when you have dealt with it. "+
		"This turn has no person watching: do not post to Slack unless the message asks for it.",
		strings.Join(from, ", "), subject, more, thread)
}

// WakeRound wakes each running, idle agent with unread messages it was not yet woken for.
func (b *Bus) WakeRound() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.o.Wake == nil {
		return
	}
	now := b.o.Now()
	rows, err := b.o.DB.Query(`SELECT d.agent, m.id, m.thread, m.sender, m.subject FROM deliveries d JOIN messages m ON m.id = d.message
		WHERE d.read_at IS NULL AND d.woken_at IS NULL AND d.wake_tries < ? ORDER BY m.id`, b.o.Limits.WakeTries)
	if err != nil {
		log.Printf("bus: %v", err)
		return
	}
	type pending struct {
		ids     []int64
		thread  int64
		subject string
		from    []string
	}
	byAgent := map[string]*pending{}
	order := []string{}
	for rows.Next() {
		var agent, sender, subject string
		var id, thread int64
		if err := rows.Scan(&agent, &id, &thread, &sender, &subject); err != nil {
			rows.Close()
			log.Printf("bus: %v", err)
			return
		}
		p := byAgent[agent]
		if p == nil {
			p = &pending{thread: thread, subject: subject}
			byAgent[agent] = p
			order = append(order, agent)
		}
		p.ids = append(p.ids, id)
		if !slices.Contains(p.from, sender) {
			p.from = append(p.from, sender)
		}
	}
	rows.Close()
	if len(order) == 0 {
		return
	}
	fleet := b.roster()
	for _, id := range order {
		a, ok := fleet[id]
		if !ok || !a.Running || a.Busy || a.Endpoint == "" {
			continue // busy: the next round; down: at its next msg_inbox
		}
		var wakes int
		if err := b.o.DB.QueryRow(`SELECT COUNT(DISTINCT agent || '@' || woken_at) FROM deliveries WHERE woken_at >= ?`, now.Add(-time.Hour).UnixMilli()).Scan(&wakes); err != nil {
			log.Printf("bus: %v", err)
			return
		}
		if wakes >= b.o.Limits.WakesPerHour {
			_ = b.o.DB.RaiseFinding("core", "bus_wake_cap", "high", "hour "+now.UTC().Format("2006-01-02T15"), now.UnixMilli())
			return
		}
		p := byAgent[id]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		err := b.o.Wake(ctx, a, wakeInput(p.from, p.subject, len(p.ids), p.thread), fmt.Sprintf("swarm-bus-%d", p.thread),
			fmt.Sprintf("swarm-bus-%s-%d", id, p.ids[len(p.ids)-1]))
		cancel()
		stamp := now.UnixMilli()
		for _, mid := range p.ids {
			if err != nil {
				_, _ = b.o.DB.Exec(`UPDATE deliveries SET wake_tries = wake_tries + 1, wake_error = ? WHERE message = ? AND agent = ?`, clip(err.Error(), 200), mid, id)
			} else {
				_, _ = b.o.DB.Exec(`UPDATE deliveries SET woken_at = ?, wake_error = '' WHERE message = ? AND agent = ?`, stamp, mid, id)
			}
		}
		if err != nil {
			log.Printf("bus: waking %s: %v", id, err)
		}
	}
}

func optIso(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return isoMs(v.Int64)
}

func (b *Bus) scanMessages(rows *sql.Rows, withBody bool) ([]Message, error) {
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var att string
		var sent int64
		var read, acked sql.NullInt64
		if err := rows.Scan(&m.ID, &m.Thread, &m.From, &m.To, &m.Subject, &m.Body, &m.Priority, &att, &m.Hop, &sent, &read, &acked); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(att), &m.Attach)
		if m.Attach == nil {
			m.Attach = []string{}
		}
		m.Sent, m.Read, m.Acked = isoMs(sent), optIso(read), optIso(acked)
		if !withBody {
			m.Body = clip(m.Body, 160)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

const msgCols = `m.id, m.thread, m.sender, m.audience, m.subject, m.body, m.priority, m.attach, m.hop, m.created`

// Inbox is the agent's received messages, newest first, with a short body preview.
func (b *Bus) Inbox(agent string, unreadOnly bool, limit int) ([]Message, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	q := `SELECT ` + msgCols + `, d.read_at, d.acked_at FROM deliveries d JOIN messages m ON m.id = d.message WHERE d.agent = ?`
	if unreadOnly {
		q += ` AND d.read_at IS NULL`
	}
	rows, err := b.o.DB.Query(q+` ORDER BY m.id DESC LIMIT ?`, agent, limit)
	if err != nil {
		return nil, err
	}
	return b.scanMessages(rows, false)
}

func (b *Bus) recipients(id int64) ([]Recipient, error) {
	rows, err := b.o.DB.Query(`SELECT agent, read_at, acked_at, woken_at FROM deliveries WHERE message = ? ORDER BY agent`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Recipient{}
	for rows.Next() {
		var r Recipient
		var read, acked, woken sql.NullInt64
		if err := rows.Scan(&r.Agent, &read, &acked, &woken); err != nil {
			return nil, err
		}
		r.Read, r.Acked, r.Woken = optIso(read), optIso(acked), optIso(woken)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Read is one message the agent sent or received, in full; a recipient's read marks it read.
func (b *Bus) Read(agent string, id int64) (*Message, error) {
	rows, err := b.o.DB.Query(`SELECT `+msgCols+`, d.read_at, d.acked_at FROM messages m LEFT JOIN deliveries d ON d.message = m.id AND d.agent = ?
		WHERE m.id = ? AND (m.sender = ? OR d.agent IS NOT NULL)`, agent, id, agent)
	if err != nil {
		return nil, err
	}
	ms, err := b.scanMessages(rows, true)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, toolErr("no message %d for you", id)
	}
	m := ms[0]
	if m.From == agent {
		if m.Recipients, err = b.recipients(id); err != nil {
			return nil, err
		}
	} else if m.Read == "" {
		now := b.o.Now().UnixMilli()
		if _, err := b.o.DB.Exec(`UPDATE deliveries SET read_at = ? WHERE message = ? AND agent = ? AND read_at IS NULL`, now, id, agent); err != nil {
			return nil, err
		}
		m.Read = isoMs(now)
	}
	return &m, nil
}

// Ack marks a received message dealt with (and read). Acks wake no one.
func (b *Bus) Ack(agent string, id int64) error {
	now := b.o.Now().UnixMilli()
	res, err := b.o.DB.Exec(`UPDATE deliveries SET acked_at = COALESCE(acked_at, ?), read_at = COALESCE(read_at, ?) WHERE message = ? AND agent = ?`, now, now, id, agent)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return toolErr("message %d was not sent to you", id)
	}
	return nil
}

// Thread is every message of the thread of id that the agent sent or received, oldest first.
func (b *Bus) Thread(agent string, id int64) ([]Message, error) {
	var root int64
	if err := b.o.DB.QueryRow(`SELECT thread FROM messages WHERE id = ?`, id).Scan(&root); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, toolErr("no message %d", id)
		}
		return nil, err
	}
	rows, err := b.o.DB.Query(`SELECT `+msgCols+`, d.read_at, d.acked_at FROM messages m LEFT JOIN deliveries d ON d.message = m.id AND d.agent = ?
		WHERE m.thread = ? AND (m.sender = ? OR d.agent IS NOT NULL) ORDER BY m.id`, agent, root, agent)
	if err != nil {
		return nil, err
	}
	ms, err := b.scanMessages(rows, true)
	if err != nil {
		return nil, err
	}
	if len(ms) == 0 {
		return nil, toolErr("thread %d is not one you are part of", root)
	}
	return ms, nil
}

// OwnerThread is a thread as the owner sees it: every message with its recipients.
type OwnerThread struct {
	Thread   int64     `json:"thread"`
	Subject  string    `json:"subject"`
	Updated  string    `json:"updated"`
	Messages []Message `json:"messages"`
}

// Threads are the newest threads with every message (bodies when withBody), for the owner.
// agent, when set, keeps the threads it sent or received a message in.
func (b *Bus) Threads(agent string, limit int, withBody bool) ([]OwnerThread, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	q := `SELECT thread, MAX(created) FROM messages m`
	args := []any{}
	if agent != "" {
		q += ` WHERE m.sender = ? OR EXISTS (SELECT 1 FROM deliveries d WHERE d.message = m.id AND d.agent = ?)`
		args = append(args, agent, agent)
	}
	rows, err := b.o.DB.Query(q+` GROUP BY thread ORDER BY MAX(created) DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	type th struct{ id, at int64 }
	ths := []th{}
	for rows.Next() {
		var t th
		if err := rows.Scan(&t.id, &t.at); err != nil {
			rows.Close()
			return nil, err
		}
		ths = append(ths, t)
	}
	rows.Close()
	out := []OwnerThread{}
	for _, t := range ths {
		mrows, err := b.o.DB.Query(`SELECT `+msgCols+`, NULL, NULL FROM messages m WHERE m.thread = ? ORDER BY m.id`, t.id)
		if err != nil {
			return nil, err
		}
		ms, err := b.scanMessages(mrows, withBody)
		if err != nil {
			return nil, err
		}
		for i := range ms {
			if ms[i].Recipients, err = b.recipients(ms[i].ID); err != nil {
				return nil, err
			}
			if !withBody {
				ms[i].Body, ms[i].Subject = "", ""
			}
		}
		subject := ""
		if len(ms) > 0 && withBody {
			subject = ms[0].Subject
		}
		out = append(out, OwnerThread{Thread: t.id, Subject: subject, Updated: isoMs(t.at), Messages: ms})
	}
	return out, nil
}

// BusCounts are an agent's message counts (no subjects or bodies: the office shows these).
type BusCounts struct {
	Unread int `json:"unread"`
	// Received and sent in the last 24 hours.
	Received int `json:"received"`
	Sent     int `json:"sent"`
}

// Counts are every agent's counts that has any.
func (b *Bus) Counts() (map[string]BusCounts, error) {
	out := map[string]BusCounts{}
	day := b.o.Now().Add(-24 * time.Hour).UnixMilli()
	rows, err := b.o.DB.Query(`SELECT d.agent, SUM(d.read_at IS NULL), SUM(m.created >= ?) FROM deliveries d JOIN messages m ON m.id = d.message GROUP BY d.agent`, day)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a string
		var c BusCounts
		if err := rows.Scan(&a, &c.Unread, &c.Received); err != nil {
			rows.Close()
			return nil, err
		}
		out[a] = c
	}
	rows.Close()
	rows, err = b.o.DB.Query(`SELECT sender, COUNT(*) FROM messages WHERE created >= ? GROUP BY sender`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			return nil, err
		}
		c := out[a]
		c.Sent = n
		out[a] = c
	}
	return out, rows.Err()
}

// Prune deletes threads with no message newer than retain.
func (b *Bus) Prune(retain time.Duration) error {
	_, err := b.o.DB.Exec(`DELETE FROM messages WHERE thread IN (SELECT thread FROM messages GROUP BY thread HAVING MAX(created) < ?)`,
		b.o.Now().Add(-retain).UnixMilli())
	return err
}

// BusTools are the msg_* MCP tools.
func BusTools(b *Bus) []MCPTool {
	return []MCPTool{
		{
			Name: "msg_send",
			Description: "Send a message to another agent of this swarm (not to people: they are on Slack). `to` is an agent id, " +
				"`unit:<id>` for every agent of a unit, or `group` for all agents. Reply in a conversation by passing `thread` " +
				"(any message id in it). An idle recipient is woken to read it. Attach files by their shared-space path " +
				"(/shared/group/…; a unit's own layer only to agents of that unit), never by pasting contents. Cross-unit messages " +
				"follow the group rules: no client personal data. Use priority `urgent` only when it cannot wait for the next turn.",
			InputSchema: schema([]string{"to", "subject", "body"}, map[string]any{
				"to":       str("agent id, unit:<id> or group"),
				"subject":  str("one line, up to 200 characters"),
				"body":     str("the message, up to 16 KB"),
				"thread":   integer("a message id of the conversation this answers"),
				"priority": map[string]any{"type": "string", "enum": []string{"normal", "urgent"}},
				"attach":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "shared-space paths"},
			}),
			Run: func(c ToolCall) (any, error) {
				var req SendRequest
				if err := decodeArgs(c, &req); err != nil {
					return nil, err
				}
				return b.Send(c.Agent, req)
			},
		},
		{
			Name:        "msg_inbox",
			Description: "Your swarm messages, newest first, with a short preview of each body. Read one in full with msg_read.",
			InputSchema: schema(nil, map[string]any{
				"unread_only": boolean("only messages you have not read (default true)"),
				"limit":       integer("at most this many (default 20, up to 100)"),
			}),
			Run: func(c ToolCall) (any, error) {
				args := struct {
					UnreadOnly *bool `json:"unread_only"`
					Limit      int   `json:"limit"`
				}{}
				if err := decodeArgs(c, &args); err != nil {
					return nil, err
				}
				unread := args.UnreadOnly == nil || *args.UnreadOnly
				ms, err := b.Inbox(c.Agent, unread, args.Limit)
				if err != nil {
					return nil, err
				}
				return map[string]any{"messages": ms}, nil
			},
		},
		{
			Name:        "msg_read",
			Description: "Read one message you sent or received, in full. Reading a received message marks it read.",
			InputSchema: schema([]string{"id"}, map[string]any{"id": integer("message id")}),
			Run: func(c ToolCall) (any, error) {
				var args struct {
					ID int64 `json:"id"`
				}
				if err := decodeArgs(c, &args); err != nil {
					return nil, err
				}
				return b.Read(c.Agent, args.ID)
			},
		},
		{
			Name:        "msg_ack",
			Description: "Mark a received message as dealt with. It notifies no one.",
			InputSchema: schema([]string{"id"}, map[string]any{"id": integer("message id")}),
			Run: func(c ToolCall) (any, error) {
				var args struct {
					ID int64 `json:"id"`
				}
				if err := decodeArgs(c, &args); err != nil {
					return nil, err
				}
				if err := b.Ack(c.Agent, args.ID); err != nil {
					return nil, err
				}
				return map[string]any{"acked": args.ID}, nil
			},
		},
		{
			Name:        "msg_thread",
			Description: "The whole conversation a message belongs to (the messages you sent or received in it), oldest first.",
			InputSchema: schema([]string{"id"}, map[string]any{"id": integer("any message id in the thread")}),
			Run: func(c ToolCall) (any, error) {
				var args struct {
					ID int64 `json:"id"`
				}
				if err := decodeArgs(c, &args); err != nil {
					return nil, err
				}
				ms, err := b.Thread(c.Agent, args.ID)
				if err != nil {
					return nil, err
				}
				return map[string]any{"messages": ms}, nil
			},
		},
	}
}
