package luau

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/memory"
)

// The memory archive's bindings (docs/needletail.md §5.3): search_memory (memory:read) and
// save_memory, update_memory, delete_memory (memory:write). A script works on the archive only:
// it can never put text into the system prompt (no hot saves, no pins) nor change or delete a hot
// record (those are the model's). read_memory (the hot entries, fs.read:home) is unchanged.

// strList reads a script's list of strings: an array, or nil and {} (an empty Luau table).
func strList(raw json.RawMessage, what string) ([]string, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "{}" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, errf("bad_args", "%s must be a list of strings", what)
	}
	return out, nil
}

// object reads a script's options table (nil or {} for none) into v.
func object(raw json.RawMessage, v any, what string) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return errf("bad_args", "%s must be a table of options", what)
	}
	return nil
}

func (c *Call) memory(binding, op string) (*memory.Store, error) {
	if !c.Caps.Memory(op) {
		return nil, c.deny(binding, "memory:"+op)
	}
	if c.rt.Memory == nil {
		return nil, errf("unavailable", "%s: the memory is not available", binding)
	}
	return c.rt.Memory, nil
}

// memoryErr is a store refusal as the script sees it.
func memoryErr(binding string, err error) error {
	switch {
	case errors.Is(err, memory.ErrNotFound):
		return errf("not_found", "%s: %v", binding, err)
	case errors.Is(err, memory.ErrHotRecord):
		return errf("refused", "%s: %v", binding, err)
	}
	return errf("bad_args", "%s: %v", binding, err)
}

// noSecret refuses text holding a secret value the runtime knows (its redactor's, and the
// http_secrets values).
func (c *Call) noSecret(binding string, texts ...string) error {
	for _, t := range texts {
		if t != "" && c.rt.redact(t) != t {
			return errf("refused", "%s: the text holds a secret value", binding)
		}
	}
	return nil
}

func memorySearch(_ context.Context, c *Call, in json.RawMessage) (any, error) {
	mem, err := c.memory("memory.search", "read")
	if err != nil {
		return nil, err
	}
	var a struct {
		Query any             `json:"query"`
		Opts  json.RawMessage `json:"opts"`
	}
	_ = json.Unmarshal(in, &a)
	var o struct {
		IDs            json.RawMessage `json:"ids"`
		Kind           string          `json:"kind"`
		Tags           json.RawMessage `json:"tags"`
		Scope          string          `json:"scope"`
		Match          string          `json:"match"`
		Since          string          `json:"since"`
		IncludeExpired bool            `json:"include_expired"`
		Limit          int             `json:"limit"`
	}
	if err := object(a.Opts, &o, "search_memory's opts"); err != nil {
		return nil, err
	}
	q, ok := a.Query.(string)
	if a.Query != nil && !ok {
		return nil, errf("bad_args", "search_memory: query must be a string")
	}
	ids, err := strList(o.IDs, "opts.ids")
	if err != nil {
		return nil, err
	}
	tags, err := strList(o.Tags, "opts.tags")
	if err != nil {
		return nil, err
	}
	if len(ids) > memory.MaxLimit {
		return nil, errf("bad_args", "search_memory: at most %d ids", memory.MaxLimit)
	}
	res, err := mem.Search(memory.Query{Text: q, IDs: ids, Kind: o.Kind, Tags: tags, Scope: o.Scope, Match: o.Match, Since: o.Since,
		IncludeExpired: o.IncludeExpired, Limit: o.Limit})
	if err != nil {
		return nil, memoryErr("search_memory", err)
	}
	return res, nil
}

func memorySave(_ context.Context, c *Call, in json.RawMessage) (any, error) {
	mem, err := c.memory("memory.save", "write")
	if err != nil {
		return nil, err
	}
	var a struct {
		Text any             `json:"text"`
		Opts json.RawMessage `json:"opts"`
	}
	_ = json.Unmarshal(in, &a)
	text, _ := a.Text.(string)
	if strings.TrimSpace(text) == "" {
		return nil, errf("bad_args", "save_memory needs a text")
	}
	var o struct {
		Kind       string          `json:"kind"`
		Tags       json.RawMessage `json:"tags"`
		Importance int             `json:"importance"`
		Expires    string          `json:"expires"`
		Force      bool            `json:"force"`
		Hot        bool            `json:"hot"`
	}
	if err := object(a.Opts, &o, "save_memory's opts"); err != nil {
		return nil, err
	}
	if o.Hot {
		return nil, errf("refused", "save_memory saves to the archive only: what enters the always-shown memory is the model's choice (memory_manage pin)")
	}
	tags, err := strList(o.Tags, "opts.tags")
	if err != nil {
		return nil, err
	}
	if err := c.noSecret("save_memory", append([]string{text}, tags...)...); err != nil {
		return nil, err
	}
	res, err := mem.Save(memory.SaveOptions{Text: text, Kind: o.Kind, Tags: tags, Importance: o.Importance, Expires: o.Expires, Force: o.Force,
		Src: "luau:" + c.Tool})
	if err != nil {
		return nil, memoryErr("save_memory", err)
	}
	if !c.Caps.Memory("read") {
		// the archive's texts are memory:read's: a writer learns only the ids
		for i := range res.Similar {
			res.Similar[i].Text = ""
		}
	}
	return res, nil
}

func memoryUpdate(_ context.Context, c *Call, in json.RawMessage) (any, error) {
	mem, err := c.memory("memory.update", "write")
	if err != nil {
		return nil, err
	}
	var a struct {
		ID     any             `json:"id"`
		Fields json.RawMessage `json:"fields"`
	}
	_ = json.Unmarshal(in, &a)
	id, _ := a.ID.(string)
	if id == "" {
		return nil, errf("bad_args", "update_memory needs an id and a fields table")
	}
	var f map[string]json.RawMessage
	if err := object(a.Fields, &f, "update_memory's fields"); err != nil {
		return nil, err
	}
	var ch memory.Change
	for k, v := range f {
		var err error
		switch k {
		case "text":
			ch.Text = new(string)
			err = json.Unmarshal(v, ch.Text)
		case "kind":
			ch.Kind = new(string)
			err = json.Unmarshal(v, ch.Kind)
		case "tags":
			var tags []string
			tags, err = strList(v, "fields.tags")
			if tags == nil {
				tags = []string{}
			}
			ch.Tags = &tags
		case "importance":
			ch.Importance = new(int)
			err = json.Unmarshal(v, ch.Importance)
		case "expires":
			ch.Expires = new(string)
			err = json.Unmarshal(v, ch.Expires)
		case "hot":
			return nil, errf("refused", "update_memory cannot pin or unpin: what enters the always-shown memory is the model's choice")
		default:
			return nil, errf("bad_args", "update_memory: no field %q (text, kind, tags, importance, expires)", k)
		}
		if err != nil {
			var he *hostError
			if errors.As(err, &he) {
				return nil, err
			}
			return nil, errf("bad_args", "update_memory: %s has the wrong type", k)
		}
	}
	texts := []string{}
	if ch.Text != nil {
		texts = append(texts, *ch.Text)
	}
	if ch.Tags != nil {
		texts = append(texts, *ch.Tags...)
	}
	if err := c.noSecret("update_memory", texts...); err != nil {
		return nil, err
	}
	v, err := mem.Update(id, ch, true)
	if err != nil {
		return nil, memoryErr("update_memory", err)
	}
	return v, nil
}

func memoryDelete(_ context.Context, c *Call, in json.RawMessage) (any, error) {
	mem, err := c.memory("memory.delete", "write")
	if err != nil {
		return nil, err
	}
	var a struct {
		ID any `json:"id"`
	}
	_ = json.Unmarshal(in, &a)
	id, _ := a.ID.(string)
	if id == "" {
		return nil, errf("bad_args", "delete_memory needs an id")
	}
	if err := mem.Delete(id, true); err != nil {
		return nil, memoryErr("delete_memory", err)
	}
	return map[string]any{"deleted": id}, nil
}
