package luau

import (
	"context"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/followup"
	"github.com/camfinc/stormo/internal/needletail/messaging"
)

var fuRW = []string{"followup:read", "followup:write"}

// follow_up and nt.followup: the run's conversation's follow-ups (session s1 here), behind
// followup:write and followup:read (declared and granted, neither implying the other); another
// target needs its message: cap; a dry run never schedules; a before_send handler cannot.
func TestFollowUpBindings(t *testing.T) {
	rt, _ := newRuntime(t)
	q := followup.New(followup.Options{Home: rt.Home, Origin: func(s string) followup.Conversation {
		return followup.Conversation{Session: s, Source: "api_server"}
	}})
	q.SetTargetCheck(func(target string, _ followup.Conversation) error {
		if target == "slack:C0SECRET01" {
			return messaging.ErrRefused
		}
		return nil
	})
	fails(t, rt, fuRW, `return follow_up{delay = "10m", text = "x"}`, "denied") // declared, not granted
	rt.Grants = append(rt.Grants, "followup:write")
	fails(t, rt, fuRW, `return follow_up{delay = "10m", text = "x"}`, "unavailable") // no queue (luau check)
	rt.FollowUps = q
	fails(t, rt, []string{"followup:write"}, `return nt.followup.list()`, "denied")
	rt.Grants = append(rt.Grants, "followup:read")
	res, _ := exec(t, rt, fuRW, `
		local a = follow_up{delay = "2h", text = "Did the quote arrive?"}
		local okb = pcall(follow_up, {at = "2099-01-01T09:00:00Z", prompt = "x"})
		local ok, e = pcall(follow_up, {["in"] = "1s", text = "x"})
		local list = nt.followup.list()
		local c = nt.followup.cancel(a.id)
		local ok2, e2 = pcall(nt.followup.cancel, "fu-nope")
		return {a.kind, a.target, string.sub(a.due_local, -5), ok, string.match(e, "^bad_args") ~= nil, #list, list[1].id == a.id, c.cancelled == a.id, ok2, string.match(e2, "^not_found") ~= nil, okb == false}`)
	wantValue(t, res, `["message", "origin", "(UTC)", false, true, 1, true, true, false, true, true]`)
	// b (2099) is past 30 days: refused, so only a was listed
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if items, _ := q.List("s1", false); len(items) != 0 {
		t.Errorf("left: %+v", items)
	}
	// another target needs its message: cap (declared and granted), then the policy
	fails(t, rt, fuRW, `return follow_up{delay = "10m", text = "x", target = "slack"}`, "denied")
	rt.Grants = append(rt.Grants, "message:slack:*")
	caps := append([]string{"message:slack:*"}, fuRW...)
	res, _ = exec(t, rt, caps, `return follow_up{delay = "10m", text = "x", target = "slack"}.target`)
	wantValue(t, res, `"slack"`)
	fails(t, rt, caps, `return follow_up{delay = "10m", text = "x", target = "slack:C0SECRET01"}`, "refused")
	items, _ := q.List("s1", false)
	if len(items) != 1 || items[0].By != "luau:t" {
		t.Errorf("made by %+v", items)
	}
	// a dry run never schedules; a before_send handler cannot
	call := rt.NewCall("t", "s1", Caps{}, true)
	if out := string(call.Call(context.Background(), "followup.create", []byte(`{"in":"10m","text":"x"}`))); !strings.Contains(out, "dry_run") {
		t.Errorf("dry run: %s", out)
	}
	c2, _ := NewCaps(fuRW, rt.Grants)
	call = rt.NewCall("t", "s1", c2, false)
	ctx := messaging.WithHandler(context.Background(), BeforeSend)
	if out := string(call.Call(ctx, "followup.create", []byte(`{"in":"10m","text":"x"}`))); !strings.Contains(out, "before_send") {
		t.Errorf("before_send: %s", out)
	}
}
