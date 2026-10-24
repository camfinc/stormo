package luau

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/followup"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

// The one-time follow-ups (docs/needletail.md §4.12, §5.3): follow_up (followup:write) and
// nt.followup.list (followup:read), nt.followup.cancel (followup:write), in the run's
// conversation, as the follow_up tool works in the turn's.

func (c *Call) followUps(binding, op string) (*followup.Queue, error) {
	if !c.Caps.FollowUp(op) {
		return nil, c.deny(binding, "followup:"+op)
	}
	if c.rt.FollowUps == nil {
		return nil, errf("unavailable", "%s: follow-ups are not available here", binding)
	}
	return c.rt.FollowUps, nil
}

// followUpErr is a refusal as the script sees it.
func followUpErr(binding string, err error) error {
	switch {
	case errors.Is(err, followup.ErrNotFound):
		return errf("not_found", "%s: %v", binding, err)
	case errors.Is(err, messaging.ErrRefused):
		return errf("refused", "%s: %v", binding, err)
	}
	return errf("bad_args", "%s: %v", binding, err)
}

func followUpCreate(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	q, err := c.followUps("followup.create", "write")
	if err != nil {
		return nil, err
	}
	var a struct {
		Delay  any `json:"delay"`
		In     any `json:"in"` // the tool's name for it; a Luau keyword, so ["in"] = …
		At     any `json:"at"`
		Text   any `json:"text"`
		Prompt any `json:"prompt"`
		Target any `json:"target"`
	}
	_ = json.Unmarshal(in, &a)
	str := func(v any) string { s, _ := v.(string); return s }
	delay := str(a.Delay)
	if delay == "" {
		delay = str(a.In)
	}
	spec := followup.Spec{In: delay, At: str(a.At), Text: str(a.Text), Prompt: str(a.Prompt), Target: str(a.Target), By: "luau:" + c.Tool}
	if t := strings.TrimSpace(spec.Target); t != "" && !strings.EqualFold(t, followup.TargetOrigin) {
		mt, err := messaging.Parse(t)
		if err != nil {
			return nil, errf("bad_args", "follow_up: %v", err)
		}
		if !c.Caps.Message(mt.String()) {
			return nil, c.deny("followup.create", "message:"+mt.String())
		}
	}
	it, err := q.Create(ctx, c.Session, spec)
	if err != nil {
		return nil, followUpErr("follow_up", err)
	}
	v := q.ViewOf(it, c.rt.now(), false)
	return map[string]any{"id": v.ID, "kind": v.Kind, "due": v.Due, "due_local": v.DueLocal, "target": v.Target}, nil
}

func followUpList(_ context.Context, c *Call, _ json.RawMessage) (any, error) {
	q, err := c.followUps("followup.list", "read")
	if err != nil {
		return nil, err
	}
	items, err := q.List(c.Session, false)
	if err != nil {
		return nil, followUpErr("nt.followup.list", err)
	}
	out := make([]followup.View, 0, len(items))
	now := c.rt.now()
	for _, it := range items {
		out = append(out, q.ViewOf(it, now, false))
	}
	return out, nil
}

func followUpCancel(_ context.Context, c *Call, in json.RawMessage) (any, error) {
	q, err := c.followUps("followup.cancel", "write")
	if err != nil {
		return nil, err
	}
	var a struct {
		ID any `json:"id"`
	}
	_ = json.Unmarshal(in, &a)
	id, _ := a.ID.(string)
	if strings.TrimSpace(id) == "" {
		return nil, errf("bad_args", "nt.followup.cancel needs the follow-up's id")
	}
	it, err := q.Cancel(c.Session, id)
	if err != nil {
		return nil, followUpErr("nt.followup.cancel", err)
	}
	return map[string]any{"cancelled": it.ID}, nil
}
