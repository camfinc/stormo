package followup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/tools"
)

// ToolName is the model's tool (sched.FollowUpTool, which sched never offers to a scheduled run;
// pinned by a test).
const ToolName = "follow_up"

const toolDescription = `Do or say something once, later, in this conversation: "say hola in 10 seconds", "remind me tomorrow at 9", "check back on the quote in 2 hours".

action 'create' with when, in (a delay: "10s", "90m", "2h", "3d") or at (a time on your clock: "tomorrow 9am", "monday 14:00", "21:00", RFC 3339), and what: text (sent as it is, no turn) or prompt (you get a turn then, in this conversation, and your reply is posted here). At least 5 seconds and at most 30 days ahead. 'list' shows this conversation's follow-ups; 'cancel' removes one by id. The result says when it is due on your clock: tell the person that time. Recurring work is cronjob_manage's.`

const toolSchema = `{"type":"object","properties":{
"action":{"type":"string","enum":["create","list","cancel"],"description":"create (default), list or cancel."},
"in":{"type":"string","description":"create: a delay, \"10s\", \"90m\", \"2h\", \"3d\" (or \"1h30m\")."},
"at":{"type":"string","description":"create, instead of in: a time on your clock, \"tomorrow 9am\", \"monday 14:00\", \"21:00\", \"2026-10-25 09:00\" or RFC 3339."},
"text":{"type":"string","description":"create: the message, sent as it is at that time (Markdown, at most 4 KB)."},
"prompt":{"type":"string","description":"create, instead of text: what you will do then, in a turn of your own in this conversation (at most 4 KB). Your reply is what is posted."},
"target":{"type":"string","description":"create, optional: where to post instead of this conversation, as send_message takes it ('slack', 'slack:<channel>[:<thread_ts>]', 'slack:<user>'); held to the same rules."},
"id":{"type":"string","description":"cancel: the follow-up's id."},
"all":{"type":"boolean","description":"list: every conversation's (the owner's own turns only)."}
}}`

type toolArgs struct {
	Action string `json:"action"`
	In     string `json:"in"`
	At     string `json:"at"`
	Text   string `json:"text"`
	Prompt string `json:"prompt"`
	Target string `json:"target"`
	ID     string `json:"id"`
	All    bool   `json:"all"`
}

// View is a follow-up as the tool and Luau show it: its due time on the agent's clock.
type View struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Due      string `json:"due"`
	DueLocal string `json:"due_local"`
	In       string `json:"in"`
	Status   string `json:"status"`
	Target   string `json:"target"`
	Text     string `json:"text,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Error    string `json:"last_error,omitempty"`
	Late     bool   `json:"late,omitempty"`
	// Conversation is shown in an every-conversation list.
	Conversation string `json:"conversation,omitempty"`
}

// ViewOf is it as shown at now.
func (q *Queue) ViewOf(it Item, now time.Time, withConversation bool) View {
	due := it.DueAt()
	v := View{ID: it.ID, Kind: it.Kind, Due: due.In(q.o.Location).Format(time.RFC3339), DueLocal: Local(due, q.o.Location),
		In: Relative(due.Sub(now)), Status: it.Status, Target: it.Target, Text: clip(it.Text, 300), Prompt: clip(it.Prompt, 300),
		Attempts: it.Attempts, Error: it.LastError, Late: it.Late}
	if withConversation {
		v.Conversation = it.Origin.Session
	}
	return v
}

// Relative is a duration as "in 10s", "in 1h30m", "in 3d2h" (its two largest units), or
// "5m ago".
func Relative(d time.Duration) string {
	ago := d < 0
	if ago {
		d = -d
	}
	secs := int64(d.Round(time.Second) / time.Second)
	parts := []struct {
		n    int64
		unit string
	}{{secs / 86400, "d"}, {secs % 86400 / 3600, "h"}, {secs % 3600 / 60, "m"}, {secs % 60, "s"}}
	s := "0s"
	for i, p := range parts {
		if p.n == 0 {
			continue
		}
		s = fmt.Sprint(p.n, p.unit)
		if i+1 < len(parts) && parts[i+1].n != 0 {
			s += fmt.Sprint(parts[i+1].n, parts[i+1].unit)
		}
		break
	}
	if ago {
		return s + " ago"
	}
	return "in " + s
}

// Tool is follow_up for the model: create, list and cancel this conversation's follow-ups.
func (q *Queue) Tool() tools.Tool {
	return tools.Tool{Name: ToolName, Description: toolDescription, Schema: json.RawMessage(toolSchema), HookArgs: hookArgs,
		Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (any, error) {
			var a toolArgs
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("bad arguments: %v", err)
			}
			session := ""
			if env != nil {
				session = env.Session
			}
			return q.Act(ctx, session, a.Action, Spec{In: a.In, At: a.At, Text: a.Text, Prompt: a.Prompt, Target: a.Target, By: "model"}, a.ID, a.All)
		}}
}

// Act is one call of the tool (and of Luau's bindings): create, list or cancel.
func (q *Queue) Act(ctx context.Context, session, action string, spec Spec, id string, all bool) (any, error) {
	now := q.o.Now()
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", "create":
		it, err := q.Create(ctx, session, spec)
		if err != nil {
			return nil, err
		}
		v := q.ViewOf(it, now, false)
		where := "in this conversation"
		if it.Target != TargetOrigin {
			where = "to " + it.Target
		}
		what := "the message is sent"
		if it.Kind == KindPrompt {
			what = "you get a turn"
		}
		return map[string]any{"id": v.ID, "kind": v.Kind, "due": v.Due, "due_local": v.DueLocal, "in": v.In, "target": v.Target,
			"note": fmt.Sprintf("Scheduled: %s %s at %s (%s). Tell them that time; follow_up cancel %s undoes it.", what, where, v.DueLocal, v.In, v.ID)}, nil
	case "list":
		items, err := q.List(session, all)
		if err != nil {
			return nil, err
		}
		out := make([]View, 0, len(items))
		for _, it := range items {
			out = append(out, q.ViewOf(it, now, all))
		}
		return map[string]any{"followups": out, "count": len(out), "now": Local(now, q.o.Location)}, nil
	case "cancel":
		if strings.TrimSpace(id) == "" {
			return nil, errors.New("cancel needs the follow-up's id (follow_up list shows them)")
		}
		it, err := q.Cancel(session, id)
		if err != nil {
			return nil, err
		}
		return map[string]any{"cancelled": it.ID, "was_due": Local(it.DueAt(), q.o.Location)}, nil
	}
	return nil, fmt.Errorf("unknown action %q: create, list or cancel", action)
}

// hookArgs is the call as the hooks see it: metadata only, never the text, prompt or target.
func hookArgs(raw string) string {
	var a toolArgs
	_ = json.Unmarshal([]byte(raw), &a)
	out := map[string]any{"action": strings.ToLower(strings.TrimSpace(orDefault(a.Action, "create"))), "has_in": a.In != "", "has_at": a.At != "",
		"has_text": a.Text != "", "has_prompt": a.Prompt != "", "has_target": a.Target != ""}
	if a.ID != "" {
		out["id"] = clip(a.ID, 64)
	}
	if a.All {
		out["all"] = true
	}
	b, _ := json.Marshal(out)
	return string(b)
}

func orDefault(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}
