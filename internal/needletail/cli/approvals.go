package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/api"
	"github.com/camfinc/stormo/internal/needletail/approvals"
	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sched"
	"github.com/camfinc/stormo/internal/needletail/sessions"
	"github.com/camfinc/stormo/internal/needletail/slack"
	"github.com/camfinc/stormo/internal/needletail/tools"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// approvalTick is how often pending approvals are checked for their timeout (tests shorten it).
var approvalTick = 30 * time.Second

// userIDRe-like check for an approver: a Slack user id.
func slackUserID(s string) bool {
	if len(s) < 9 || s[0] != 'U' && s[0] != 'W' {
		return false
	}
	for _, r := range s[1:] {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// approvers are who decide (docs/needletail.md §7): approvals.approvers within allowed_users (one
// it does not list is dropped, logged), else the first user allowed_users names. Never "*", never
// a bot.
func approvers(c *wire.ApprovalsConfig, allowed []string, logf func(string)) []string {
	listed := []string{}
	for _, u := range allowed {
		if u != "*" && slackUserID(u) {
			listed = append(listed, u)
		}
	}
	if c == nil || len(c.Approvers) == 0 {
		if len(listed) == 0 {
			return nil
		}
		return listed[:1]
	}
	out := []string{}
	for _, u := range c.Approvers {
		if !slices.Contains(listed, u) {
			logf(fmt.Sprintf("approvals: approver %s is not in allowed_users: left out", u))
			continue
		}
		if !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// riskyCommands compiles approvals.commands over the defaults; a pattern that does not compile
// (the adapter checks them) leaves the defaults alone, logged.
func riskyCommands(c *wire.ApprovalsConfig, logf func(string)) *approvals.Commands {
	var extra []approvals.Rule
	if c != nil {
		extra = c.Commands
	}
	cmds, err := approvals.CompileCommands(extra)
	if err != nil {
		logf(fmt.Sprintf("approvals: %v: only the default risky commands apply", err))
		cmds, _ = approvals.CompileCommands(nil)
	}
	return cmds
}

// startApprovals is serve's approvals service: its store loaded (what fell due while the runtime
// was down denied), approved actions run by the runtime, outcomes noted in their conversations,
// timeouts checked every approvalTick until ctx ends. The Slack gateway shows the cards (wired
// once it runs).
func startApprovals(ctx context.Context, rt *runtime, srv *api.Server, gw *slack.Gateway) *approvals.Service {
	c := rt.cfg.Approvals
	o := approvals.Options{StateDir: rt.cfg.StateDir(), TmpDir: rt.cfg.TmpDir(), Approvers: approvers(c, slackAllowedUsers(), rt.logf), Log: rt.logf,
		Execute: map[string]approvals.Executor{approvals.KindTool: rt.runApproved, approvals.KindDelivery: rt.sched.DeliverApproved},
		// "trust thread": serialized with the conversation's turns (as the API's trust), and only
		// as the conversation stood when the agent asked (sessions.Store.TrustAt)
		Trust: func(r *approvals.Request, by string) error {
			if r.TrustAt == nil {
				return errors.New("the request kept no record of where the conversation stood")
			}
			release, err := holdWithin(srv, r.Conversation, trustWait)
			if err != nil {
				return fmt.Errorf("a turn kept running in it (%v)", err)
			}
			defer release()
			return rt.store.TrustAt(r.Conversation, by, sessions.TrustPoint{Session: r.TrustAt.Session, Marks: r.TrustAt.Marks, Epoch: r.TrustAt.Epoch})
		},
		Note: func(r *approvals.Request, text string) error {
			release, err := srv.Hold(r.Conversation)
			if err != nil {
				return err // a turn runs there: its next one takes the note (Agent.Notes)
			}
			defer release()
			tip, ok := rt.store.Tip(r.Conversation)
			if !ok {
				return sessions.ErrNotFound
			}
			if approvals.NoteMarked(r) {
				if err := rt.store.MarkUntrusted(tip); err != nil {
					return err // no mark, no data
				}
			}
			if _, err := rt.store.Append(tip, wire.Message{Role: "user", Kind: wire.KindApproval, Content: text, Untrusted: approvals.NoteMarked(r)}); err != nil {
				return err
			}
			return rt.store.Sync(tip)
		},
		Hook: func(event string, r *approvals.Request) {
			if h, ok := rt.agent.Observer.(interface {
				Approval(event, session, id, status string)
			}); ok {
				session := r.Conversation
				if session == "" {
					session = r.Session
				}
				h.Approval(event, session, r.ID, r.Status)
			}
		}}
	if c != nil {
		o.Timeout = time.Duration(c.Timeout) * time.Second
		o.DeliveryExpiry = time.Duration(c.DeliveryExpiryDays) * 24 * time.Hour
		o.PerConversation, o.PerDay = c.PerConversation, c.PerDay
	}
	svc := approvals.New(o)
	if gw != nil {
		svc.SetNotifier(gw)
	}
	if err := svc.Boot(ctx); err != nil {
		rt.logf(fmt.Sprintf("approvals: %v: approvals are off (nothing is approved; what needs approval is refused)", err))
		return nil
	}
	if len(o.Approvers) == 0 {
		rt.logf("approvals: no approver (approvals.approvers, or a user allowed_users names): what needs approval is refused")
	}
	rt.approvals.Store(svc)
	rt.agent.Notes = svc.Notes
	rt.sched.SetApprovals(rt.askDelivery)
	rt.sched.SetOwners(svc.Approvers)
	go func() {
		t := time.NewTicker(approvalTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				svc.Expire(ctx)
			}
		}
	}()
	return svc
}

func (rt *runtime) logf(s string) { rt.logger.Println(s) }

// trustWait is how long a "trust thread" waits for a turn running in its conversation (the
// requesting one finishing) before the trust part is refused.
var trustWait = 2 * time.Minute

// holdWithin holds conversation's turn slot (api.Server.Hold), waiting up to d for a turn
// running there.
func holdWithin(srv *api.Server, conversation string, d time.Duration) (func(), error) {
	end := time.Now().Add(d)
	for {
		release, err := srv.Hold(conversation)
		if err == nil || !errors.Is(err, api.ErrSessionBusy) || time.Now().After(end) {
			return release, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// approve puts a call a gate held for a person (tools.Env.Approve) to the approvers: a card from
// the structured call, in their DMs; the tool answers "pending approval <id>" at once. In a
// scheduled run (no person in the loop), or with approvals off, it is refused.
func (rt *runtime) approve(ctx context.Context, env *tools.Env, tool string, args json.RawMessage, need *messaging.ApprovalNeeded) (any, error) {
	if messaging.Scheduled(ctx) {
		return nil, fmt.Errorf("%v; a scheduled run has no person to approve it, so it is refused", need)
	}
	svc := rt.approvals.Load()
	if svc == nil {
		return nil, fmt.Errorf("%v, and approvals are not available here (they run in serve, with the Slack gateway): it is refused", need)
	}
	session := ""
	if env != nil {
		session = env.Session
	}
	root := session
	if r, ok := rt.store.Root(session); ok {
		root = r
	}
	origin := approvals.Origin{}
	if channel, thread, ok := slack.Origin(root); ok {
		origin = approvals.Origin{Channel: channel, Thread: thread}
		if in, ok := messaging.InputOf(ctx); ok && thread == "" {
			origin.Thread = in.Thread // a DM thread: told where it was asked
		}
	}
	// one unambiguous object: rendered, hashed and run as it is (a key twice, or keys equal
	// ignoring case, would show one value and run another)
	var props []string
	if t, ok := rt.agent.Tools.Get(tool); ok {
		props = approvals.SchemaProperties(t.Schema)
	}
	args, err := approvals.Canonical(args, props)
	if err != nil {
		return nil, fmt.Errorf("%v; its arguments were not put to a person: %v", need, err)
	}
	if len(need.Payload) > 0 {
		// the gate bound what runs (a send as before_send left it): that, canonical, is put
		if args, err = approvals.Canonical(need.Payload, props); err != nil {
			return nil, fmt.Errorf("%v; its arguments were not put to a person: %v", need, err)
		}
	}
	card, bind, err := rt.describeBound(env, tool, args)
	if err != nil {
		return nil, fmt.Errorf("%v; the request could not be shown to a person (%v), so it is refused", need, err)
	}
	tainted := rt.store.Untrusted(root) || messaging.UntrustedWhy(ctx) != ""
	var trustAt *approvals.TrustPoint
	if p, ok := rt.store.Point(root); ok && need.Trust && tainted {
		trustAt = &approvals.TrustPoint{Session: p.Session, Marks: p.Marks, Epoch: p.Epoch}
	}
	req, err := svc.Request(ctx, approvals.Ask{Action: approvals.Action{Kind: approvals.KindTool, Tool: tool, Args: args, Filtered: need.Filtered, Bind: bind}, Conversation: root, Session: session,
		Origin: origin, Card: card, Why: need.Why, Reason: messaging.ReasonOf(ctx), Taint: rt.store.Taint(root), Trust: need.Trust && tainted && trustAt != nil, TrustAt: trustAt})
	if err != nil {
		return nil, fmt.Errorf("%v; it was not put to a person: %v", need, err)
	}
	local := req.Expires
	if rt.cfg.Location != nil {
		local = local.In(rt.cfg.Location)
	}
	return map[string]any{"status": "pending_approval", "approval_id": req.ID, "expires": local.Format(time.RFC3339),
		"note": fmt.Sprintf("pending approval %s: the owner was asked in Slack. It runs only if they approve, exactly as asked, once; with no decision by %s it is denied. "+
			"Tell the person it waits for approval; do not call it again. The outcome comes back to this conversation.", req.ID, local.Format("15:04 MST"))}, nil
}

// runApproved runs an approved tool call (approvals.KindTool): the same tool with exactly the
// arguments the card carried, in the conversation it was asked in (its live session), endorsed
// (messaging.WithEndorsed: the approval, not the conversation, is what lets it run).
func (rt *runtime) runApproved(ctx context.Context, r *approvals.Request) (string, error) {
	session := r.Session
	if r.Conversation != "" {
		if tip, ok := rt.store.Tip(r.Conversation); ok {
			session = tip
		}
	}
	env := rt.agent.Env
	env.Session = session
	ctx = messaging.WithSession(messaging.WithQuarantine(ctx, nil), session)
	if r.Action.Filtered {
		ctx = messaging.WithFiltered(ctx) // posted exactly as approved: before_send already ran
	}
	ctx = messaging.WithBindings(ctx, r.Action.Bind) // only on the state the card showed
	out, failed := rt.agent.Tools.Call(ctx, &env, r.Action.Tool, string(r.Action.Args))
	if failed {
		return "", errors.New(clipText(out, 1000))
	}
	return clipText(out, 2000), nil
}

// askDelivery puts a tainted scheduled run's report to the approvers (the scheduler's held
// delivery): Approve sends exactly that report to the job's targets, Deny drops it; with no
// decision in approvals.delivery_expiry_days it is dropped.
func (rt *runtime) askDelivery(ctx context.Context, d approvals.Delivery, why string) (string, error) {
	taint := rt.store.Taint(d.Session)
	svc := rt.approvals.Load()
	if svc == nil {
		return "", errors.New("approvals are not available: the report is not delivered")
	}
	// bounded: a gateway that never connects must not hold the run's finish (and its job) forever
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	req, err := svc.Request(actx, approvals.Ask{Action: approvals.Action{Kind: approvals.KindDelivery, Delivery: &d}, Session: d.Session,
		Card: approvals.Card{Title: fmt.Sprintf("deliver the report of the scheduled job %s (%s)", orDefault(d.JobName, d.JobID), d.JobID), Target: strings.Join(d.Targets, ", "),
			Payload: d.Text, PayloadKind: "text"}, Why: why, Taint: taint})
	if err != nil {
		return "", err
	}
	return req.ID, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// describe is a call as its card shows it (describeBound without the bindings).
func (rt *runtime) describe(env *tools.Env, tool string, raw json.RawMessage) (approvals.Card, error) {
	c, _, err := rt.describeBound(env, tool, raw)
	return c, err
}

// describeBound is a call as its card shows it, from the call alone: a title, its target, and
// exactly what runs (the text sent, a diff for a file, skill, script, job or memory record, the
// command), and what the card showed of the state the call changes (a memory record's revision,
// the hot entry it replaces, a kept report's hash): the approval binds them.
func (rt *runtime) describeBound(env *tools.Env, tool string, raw json.RawMessage) (approvals.Card, map[string]string, error) {
	card, err := rt.describeCall(env, tool, raw)
	if err != nil {
		return card, nil, err
	}
	var a map[string]any
	_ = json.Unmarshal(raw, &a)
	str := func(k string) string { s, _ := a[k].(string); return s }
	bind := map[string]string{}
	switch {
	case tool == "memory_manage" && str("id") != "" && env != nil && env.Memory != nil:
		rec, rev, err := env.Memory.Record(env.Memory.AudienceOf(env.Session), str("id"))
		if err != nil {
			break // not found here: the call fails the same way when it runs
		}
		bind["record_revision"] = rev
		card.Target = fmt.Sprintf("memory record %s (revision %s)", rec.ID, rev)
		head := fmt.Sprintf("kind %s, importance %d, scope %s", rec.Kind, rec.Imp, orDefault(rec.Scope, "from its source"))
		if rec.Untrusted {
			head += ", marked untrusted (saved from words the agent may not obey)"
		}
		switch str("action") {
		case "update":
			after := rec.Text
			if t, ok := a["text"].(string); ok {
				after = t
			}
			card.Payload, card.PayloadKind = sched.UnifiedDiff(rec.Text, after), "diff"
			if rest := pretty(without(a, "action", "id", "text")); rest != "{}" {
				card.Payload += "\nother changes: " + rest
			}
		case "delete":
			card.Payload, card.PayloadKind = sched.UnifiedDiff(rec.Text, ""), "diff"
		default: // pin, unpin
			card.Payload, card.PayloadKind = rec.Text, "text"
		}
		card.Payload = "(" + head + ")\n" + card.Payload
	case tool == "memory" && env != nil && env.Memory != nil && (str("action") == "replace" || str("action") == "remove"):
		entries, err := env.Memory.Entries(str("target"))
		if err != nil {
			break
		}
		i, err := memory.Match(entries, str("old_text"))
		if err != nil {
			break // ambiguous or missing: the call fails the same way when it runs
		}
		bind["memory_entry"] = entries[i]
		card.Target = "the always-shown memory (" + str("target") + ")"
		after := ""
		if str("action") == "replace" {
			after = str("content")
		}
		card.Payload, card.PayloadKind = sched.UnifiedDiff(entries[i], after), "diff"
	case tool == sched.CronJobTool && (str("action") == "retry_delivery" || str("action") == "discard_delivery") && str("job_id") != "" && rt.sched != nil:
		text, targets, ok := rt.sched.KeptReport(str("job_id"))
		if !ok {
			break
		}
		bind["report_sha256"] = sched.ReportHash(text)
		card.Target = strings.Join(targets, ", ")
		if d := str("deliver"); d != "" {
			card.Target = d + " (replacing " + card.Target + ")"
		}
		card.Payload, card.PayloadKind = text, "text"
	}
	if len(card.Payload) > approvals.MaxPayload {
		return card, nil, approvals.ErrTooLarge
	}
	if len(bind) == 0 {
		bind = nil
	}
	return card, bind, nil
}

func pretty(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return strings.TrimSpace(b.String())
}

// without is m less keys.
func without(m map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if !slices.Contains(keys, k) {
			out[k] = v
		}
	}
	return out
}

// describeCall is a call's card from its arguments alone.
func (rt *runtime) describeCall(env *tools.Env, tool string, raw json.RawMessage) (approvals.Card, error) {
	var a map[string]any
	if err := json.Unmarshal(raw, &a); err != nil {
		return approvals.Card{}, fmt.Errorf("its arguments are not an object")
	}
	str := func(k string) string { s, _ := a[k].(string); return s }
	pretty := func(v any) string {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(v)
		return strings.TrimSpace(b.String())
	}
	card := approvals.Card{Title: "call " + tool, Payload: pretty(a), PayloadKind: "json"}
	switch {
	case tool == messaging.SendTool:
		card = approvals.Card{Title: "send a message to " + orDefault(str("target"), "the home channel"), Target: orDefault(str("target"), "slack (home)"), Payload: str("message"), PayloadKind: "text"}
		if b, _ := a["broadcast"].(bool); b {
			card.Title += " (with a broadcast)"
		}
	case tool == "write_file":
		card = approvals.Card{Title: "write the file " + rt.shownPath(env, str("path")), Target: rt.shownPath(env, str("path")), Payload: str("content"), PayloadKind: "text"}
		if old, ok := rt.currentFile(env, str("path")); ok {
			card.Payload, card.PayloadKind = sched.UnifiedDiff(old, str("content")), "diff"
		}
	case tool == "patch":
		card = approvals.Card{Title: "patch the file " + rt.shownPath(env, str("path")), Target: rt.shownPath(env, str("path")), PayloadKind: "diff",
			Payload: sched.UnifiedDiff(str("old_string"), str("new_string"))}
		if old, ok := rt.currentFile(env, str("path")); ok && strings.Contains(old, str("old_string")) && str("old_string") != "" {
			all, _ := a["replace_all"].(bool)
			n := 1
			if all {
				n = -1
			}
			card.Payload = sched.UnifiedDiff(old, strings.Replace(old, str("old_string"), str("new_string"), n))
		}
	case tool == "skill_manage":
		card.Title = fmt.Sprintf("%s the skill %s", orDefault(str("action"), "change"), str("name"))
		card.Target = "skills/" + str("name")
		switch str("action") {
		case "create":
			card.Payload, card.PayloadKind = str("content"), "text"
		case "edit":
			card.Payload, card.PayloadKind = str("content"), "text"
			if env != nil && env.Skills != nil {
				if s, err := env.Skills.Find(str("name")); err == nil {
					if old, ok := rt.currentFile(env, "skills/"+s.Dir+"/SKILL.md"); ok {
						card.Payload, card.PayloadKind = sched.UnifiedDiff(old, str("content")), "diff"
					}
				}
			}
		case "patch":
			card.Payload, card.PayloadKind = sched.UnifiedDiff(str("old_string"), str("new_string")), "diff"
		case "write_file":
			card.Target += "/" + str("file_path")
			card.Payload, card.PayloadKind = str("file_content"), "text"
		}
		if card.Payload == "" {
			card.Payload, card.PayloadKind = pretty(a), "json"
		}
	case tool == sched.CronJobTool:
		card.Title = fmt.Sprintf("%s the scheduled job %s", orDefault(str("action"), "change"), orDefault(str("job_id"), str("name")))
		if str("job_id") == "" && str("name") == "" {
			card.Title = fmt.Sprintf("%s a scheduled job (%s)", orDefault(str("action"), "change"), orDefault(str("schedule"), "no schedule given"))
		}
		if id := str("job_id"); id != "" {
			if cur, ok := rt.sched.JobJSON(id); ok {
				after := map[string]any{}
				_ = json.Unmarshal(cur, &after)
				before := pretty(after)
				for k, v := range a {
					if k != "action" && k != "job_id" {
						after[k] = v
					}
				}
				if str("action") == "remove" {
					card.Payload, card.PayloadKind = sched.UnifiedDiff(before, ""), "diff"
				} else {
					card.Payload, card.PayloadKind = sched.UnifiedDiff(before, pretty(after)), "diff"
				}
			}
		}
	case tool == "terminal":
		card = approvals.Card{Title: "run a terminal command", Target: orDefault(str("workdir"), "the home"), Payload: str("command"), PayloadKind: "command"}
		if b, _ := a["background"].(bool); b {
			card.Title += " in the background"
		}
	case tool == "follow_up":
		card.Title = "schedule a follow-up"
		card.Target = orDefault(str("target"), "this conversation")
	case tool == "memory" || tool == "memory_manage":
		card.Title = fmt.Sprintf("%s a memory", orDefault(str("action"), "change"))
		if id := str("id"); id != "" {
			card.Title += " (" + id + ")"
		}
	case strings.HasPrefix(tool, "mcp_core_"):
		card.Title = "call the core's " + strings.TrimPrefix(tool, "mcp_core_")
		if p := str("path"); p != "" {
			card.Target = p
		}
		if to := str("to"); to != "" {
			card.Target = to
		}
	}
	if len(card.Payload) > approvals.MaxPayload {
		return card, approvals.ErrTooLarge
	}
	return card, nil
}

// shownPath is a file tool's path as the card names it: where it resolves (relative to the home
// when inside it). The gate refused a path with a symlink in it, so it is the path as written,
// and the write refuses to run if it resolves elsewhere by then.
func (rt *runtime) shownPath(env *tools.Env, p string) string {
	if env == nil {
		return p
	}
	real, err := env.Sandbox.Resolve(p)
	if err != nil {
		return p
	}
	return env.Sandbox.Display(real)
}

// currentFile is a file's content as the file tools would read it now (bounded), for a diff.
func (rt *runtime) currentFile(env *tools.Env, path string) (string, bool) {
	if env == nil || path == "" {
		return "", false
	}
	real, err := env.Sandbox.Resolve(path)
	if err != nil {
		return "", false
	}
	root, rel, ok := env.Sandbox.Split(real)
	if !ok {
		return "", false
	}
	b, err := fsx.ReadFileBelow(root, rel, approvals.MaxPayload)
	if err != nil {
		return "", false // missing (a new file) or unreadable: the card shows the content whole
	}
	return string(b), true
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
