package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/camfinc/stormo/internal/needletail/memory"
)

func memoryTool() Tool {
	return Tool{
		Name: "memory",
		Description: "Keep durable notes across conversations. target \"memory\" is what you learned about the work " +
			"(facts, conventions, lessons); target \"user\" is about the people you work with (preferences, roles). " +
			"Both are shown to you at the start of every conversation. Keep entries short and specific; each file " +
			"has a size budget, so replace or remove stale entries rather than piling up new ones. " +
			"add needs content; replace needs old_text (a unique part of the entry) and content; remove needs old_text.",
		Schema: schema(`{"type":"object","properties":{
"action":{"type":"string","enum":["add","replace","remove"]},
"target":{"type":"string","enum":["memory","user"]},
"content":{"type":"string","description":"The entry's text (add, replace)."},
"old_text":{"type":"string","description":"A unique part of the entry to change (replace, remove)."}},
"required":["action","target"]}`),
		Run: func(_ context.Context, env *Env, raw json.RawMessage) (any, error) {
			var a struct {
				Action  string `json:"action"`
				Target  string `json:"target"`
				Content string `json:"content"`
				OldText string `json:"old_text"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			if env.Memory == nil {
				return nil, errors.New("memory is not available")
			}
			var u memory.Usage
			var err error
			switch a.Action {
			case "add":
				u, err = env.Memory.Add(a.Target, a.Content)
			case "replace":
				u, err = env.Memory.Replace(a.Target, a.OldText, a.Content)
			case "remove":
				u, err = env.Memory.Remove(a.Target, a.OldText)
			default:
				return nil, errors.New(`action must be "add", "replace" or "remove"`)
			}
			if err != nil {
				var ob *memory.OverBudget
				if errors.As(err, &ob) {
					return nil, fmt.Errorf("%v (now %s)", err, u)
				}
				if u.Target != "" {
					return nil, fmt.Errorf("%v (usage %s)", err, u)
				}
				return nil, err
			}
			return map[string]any{"success": true, "target": u.Target, "entries": u.Entries, "usage": u.String()}, nil
		},
	}
}
