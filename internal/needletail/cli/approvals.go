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
		Trust: func(conv, by string) error {
			return rt.store.Trust(conv, by)
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
			if _, err := rt.store.Append(tip, wire.Message{Role: "user", Kind: wire.KindApproval, Content: text}); err != nil {
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
	card, err := rt.describe(env, tool, args)
	if err != nil {
		return nil, fmt.Errorf("%v; the request could not be shown to a person (%v), so it is refused", need, err)
	}
	tainted := rt.store.Untrusted(root) || messaging.UntrustedWhy(ctx) != ""
	req, err := svc.Request(ctx, approvals.Ask{Action: approvals.Action{Kind: approvals.KindTool, Tool: tool, Args: args}, Conversation: root, Session: session,
		Origin: origin, Card: card, Why: need.Why, Reason: messaging.ReasonOf(ctx), Taint: rt.store.Taint(root), Trust: need.Trust && tainted})
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
	req, err := svc.Request(context.WithoutCancel(ctx), approvals.Ask{Action: approvals.Action{Kind: approvals.KindDelivery, Delivery: &d}, Session: d.Session,
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

// describe is a call as its card shows it, from the call alone: a title, its target, and exactly
// what runs (the text sent, a diff for a file, skill, script or job, the command).
func (rt *runtime) describe(env *tools.Env, tool string, raw json.RawMessage) (approvals.Card, error) {
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
		card = approvals.Card{Title: "write the file " + str("path"), Target: str("path"), Payload: str("content"), PayloadKind: "text"}
		if old, ok := rt.currentFile(env, str("path")); ok {
			card.Payload, card.PayloadKind = sched.UnifiedDiff(old, str("content")), "diff"
		}
	case tool == "patch":
		card = approvals.Card{Title: "patch the file " + str("path"), Target: str("path"), PayloadKind: "diff",
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
