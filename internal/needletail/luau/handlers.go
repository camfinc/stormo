package luau

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Event handlers (docs/needletail.md §5.5): .luau files in a skill's handlers/ that run before (or
// instead of) a model turn when something happens. A handler's header names the events it
// handles; for each one the file defines a function of that name (a global, or a field of the
// table the chunk returns), called with the event (JSON) and returning a decision (JSON). They
// are never offered to the model as tools.

// HandlerDir is where a skill keeps its handlers: skills/<skill>/handlers/<name>.luau.
const HandlerDir = "handlers"

var handlerFile = regexp.MustCompile(`^handlers/([a-z][a-z0-9_]{0,47})\.luau$`)

// Bounds of what a handler answers.
const (
	MaxHandlerText    = 40_000 // characters of a reply's or a rewrite's text
	MaxHandlerContext = 4 << 10
	maxContexts       = 4 // pass notes kept per event
)

// CheckFile validates a skill's .luau file as skill_manage would leave it: a tool
// (tools/<name>.luau) or a handler (handlers/<name>.luau).
func (rt *Runtime) CheckFile(ctx context.Context, rel string, body []byte) error {
	if strings.HasPrefix(rel, HandlerDir+"/") {
		return rt.CheckHandler(ctx, rel, body)
	}
	if err := rt.CheckTool(ctx, rel, body); err != nil {
		if !strings.HasPrefix(rel, ToolDir+"/") {
			return fmt.Errorf("%s: a .luau file in a skill is a tool (tools/<name>.luau) or an event handler (handlers/<name>.luau)", rel)
		}
		return err
	}
	return nil
}

// CheckHandler validates a handler: rel is its path in the skill (handlers/<name>.luau), body
// the whole file. The header must be a handler's and name the file, and the source must compile.
func (rt *Runtime) CheckHandler(ctx context.Context, rel string, body []byte) error {
	m := handlerFile.FindStringSubmatch(rel)
	if m == nil {
		return fmt.Errorf("%s: a handler lives at handlers/<name>.luau (name: a lowercase letter, then lowercase letters, digits or _)", rel)
	}
	h, err := ParseHeaderOf(body, KindHandler)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	if h.Name != m[1] {
		return fmt.Errorf("%s: the header's name %q must be the file's name %q", rel, h.Name, m[1])
	}
	if err := rt.compiles(ctx, body); err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	return nil
}

// skillFiles is every .luau file (regular, not a symlink) in a directory sub of the home's skills,
// as (skill dir, home path, real path), sorted by home path: by skill dir, then file name.
func (rt *Runtime) skillFiles(sub string) [][3]string {
	var out [][3]string
	for _, s := range skills.Index(rt.Home) {
		dir := filepath.Join(rt.Home, "skills", filepath.FromSlash(s.Dir), sub)
		if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".luau") || !e.Type().IsRegular() {
				continue
			}
			out = append(out, [3]string{s.Dir, path.Join("skills", s.Dir, sub, e.Name()), filepath.Join(dir, e.Name())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][1] < out[j][1] })
	return out
}

// HandlerFile is one handler of a skill: valid (Err nil) or not.
type HandlerFile struct {
	// Skill is the skill's dir; File the handler's path in the home
	// (skills/ops/triage/handlers/route.luau).
	Skill, File string
	// Name is <skill>__<name> (the skill dir's last segment), as logs and hooks name it.
	Name   string
	Header *Header
	Source []byte
	Err    error
}

// Handlers is every handler of the home's skills, valid or not, in the order they run: by skill
// dir, then file name. They are read again at every event, so a handler written in one
// conversation runs at the next event.
func (rt *Runtime) Handlers(ctx context.Context) []HandlerFile {
	var out []HandlerFile
	for _, f := range rt.skillFiles(HandlerDir) {
		hf := HandlerFile{Skill: f[0], File: f[1]}
		hf.Err = rt.loadHandler(ctx, &hf, f[2])
		out = append(out, hf)
	}
	return out
}

func (rt *Runtime) loadHandler(ctx context.Context, hf *HandlerFile, real string) error {
	info, err := os.Lstat(real)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if info.Size() > MaxSource {
		return fmt.Errorf("over %d bytes", MaxSource)
	}
	body, err := os.ReadFile(real)
	if err != nil {
		return err
	}
	if err := rt.CheckHandler(ctx, HandlerDir+"/"+path.Base(hf.File), body); err != nil {
		return err
	}
	h, _ := ParseHeaderOf(body, KindHandler)
	name, err := toolName(hf.Skill, h.Name)
	if err != nil {
		return err
	}
	hf.Name, hf.Header, hf.Source = name, h, body
	return nil
}

// HandlerStatus is a skill's handlers as skill_view shows them.
func (rt *Runtime) HandlerStatus(ctx context.Context, skillDir string) []skills.ToolStatus {
	out := []skills.ToolStatus{}
	for _, hf := range rt.Handlers(ctx) {
		if hf.Skill != skillDir {
			continue
		}
		st := skills.ToolStatus{File: strings.TrimPrefix(hf.File, "skills/"+skillDir+"/"), Name: hf.Name}
		if hf.Err != nil {
			st.Error = hf.Err.Error()
		} else {
			caps, _ := NewCaps(hf.Header.Caps, rt.Grants)
			st.Caps, st.Effective, st.Events, st.Jobs = hf.Header.Caps, caps.Effective(), hf.Header.Events, hf.Header.Jobs
		}
		out = append(out, st)
	}
	return out
}

// Decision is one handler's answer, checked against its event: Action is one of the event's
// actions (pass, reply or silent for a message or a schedule; allow, rewrite or block before a
// send), Text a reply's or a rewrite's text, Context a pass's note for the model.
type Decision struct {
	Action  string `json:"action"`
	Text    string `json:"text,omitempty"`
	Context string `json:"context,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// actions are each event's actions; the first is the default (what nil means).
var actions = map[string][]string{
	OnMessage:  {"pass", "reply", "silent"},
	OnMention:  {"pass", "reply", "silent"},
	OnSchedule: {"pass", "reply", "silent"},
	BeforeSend: {"allow", "rewrite", "block"},
}

// ParseDecision reads what a handler returned for event: nil (or nothing) is the default
// action; anything else is an object with an action of the event's (a string alone is taken as
// the action).
func ParseDecision(event string, value json.RawMessage) (Decision, error) {
	allowed := actions[event]
	if len(allowed) == 0 {
		return Decision{}, fmt.Errorf("%q is not an event", event)
	}
	v := strings.TrimSpace(string(value))
	if v == "" || v == "null" {
		return Decision{Action: allowed[0]}, nil
	}
	var d Decision
	var s string
	if json.Unmarshal(value, &s) == nil {
		d.Action = s
	} else {
		var raw map[string]any
		if err := json.Unmarshal(value, &raw); err != nil {
			return Decision{}, fmt.Errorf("a handler returns nil or a table {action = …}, not %s", clip(v, 80))
		}
		str := func(k string) (string, error) {
			x, ok := raw[k]
			if !ok || x == nil {
				return "", nil
			}
			s, ok := x.(string)
			if !ok {
				return "", fmt.Errorf("%s must be a string", k)
			}
			return s, nil
		}
		var err error
		if d.Action, err = str("action"); err != nil {
			return Decision{}, err
		}
		if d.Text, err = str("text"); err != nil {
			return Decision{}, err
		}
		if d.Context, err = str("context"); err != nil {
			return Decision{}, err
		}
		if d.Reason, err = str("reason"); err != nil {
			return Decision{}, err
		}
		if d.Action == "" {
			d.Action = allowed[0]
		}
	}
	known := false
	for _, a := range allowed {
		known = known || d.Action == a
	}
	if !known {
		return Decision{}, fmt.Errorf("%s returns an action of %s, not %q", event, strings.Join(allowed, ", "), d.Action)
	}
	switch d.Action {
	case "reply", "rewrite":
		if strings.TrimSpace(d.Text) == "" {
			return Decision{}, fmt.Errorf("%s needs a text", d.Action)
		}
		if utf8.RuneCountInString(d.Text) > MaxHandlerText {
			return Decision{}, fmt.Errorf("the %s's text is over %d characters", d.Action, MaxHandlerText)
		}
	case "pass":
		if len(d.Context) > MaxHandlerContext {
			return Decision{}, fmt.Errorf("the context is over %d bytes", MaxHandlerContext)
		}
	}
	if d.Action != "reply" && d.Action != "rewrite" {
		d.Text = ""
	}
	if d.Action != "pass" {
		d.Context = ""
	}
	return d, nil
}

// HookObserver hears handler runs (the core's hooks: tool_start and tool_done named
// luau:<skill>__<name>#<event>).
type HookObserver interface {
	ToolStart(session string, call wire.ToolCall)
	ToolDone(session string, call wire.ToolCall, result string, failed bool, took time.Duration)
}

// Verdict is what an event's handlers decided together (messaging.Verdict).
type Verdict = messaging.Verdict

// Dispatch runs event's handlers on ev, in order, each with its effective caps and its bound
// (limits.luau_timeout; a schedule's, limits.script_timeout) and the event in ctx (so a send from
// a before_send handler is refused, and one from any other handler meets before_send once).
//
// A message's or a schedule's handlers run until one replies or stays silent; a pass adds its
// context and goes on. Before a send, every handler sees the text the previous ones left: a
// rewrite changes it and goes on, a block stops the message. A handler that fails, passes its
// bound or answers something that is not one of the event's actions is skipped: logged, hooked
// as failed, and the event goes on as if it had passed (or allowed). serves, when set, picks the
// handlers that apply (on_schedule: the job's).
func (rt *Runtime) Dispatch(ctx context.Context, event, session string, ev map[string]any, serves func(*Header) bool) Verdict {
	v := Verdict{Action: actions[event][0]}
	if messaging.HandlerEvent(ctx) == BeforeSend && event == BeforeSend {
		return v // never reached (a before_send handler cannot send); no recursion either way
	}
	ctx = messaging.WithHandler(ctx, event)
	for _, hf := range rt.Handlers(ctx) {
		if hf.Err != nil || !hf.Header.Handles(event) || serves != nil && !serves(hf.Header) {
			continue
		}
		ev["event"] = event
		args, err := json.Marshal(ev)
		if err != nil {
			rt.logf("handler %s (%s): the event could not be encoded: %v", hf.Name, event, err)
			return v
		}
		d, err := rt.runHandler(ctx, hf, event, session, args)
		v.Ran++
		if err != nil {
			rt.logf("handler %s (%s) skipped: %v", hf.Name, event, err)
			continue
		}
		switch d.Action {
		case "pass":
			if c := strings.TrimSpace(d.Context); c != "" && len(v.Context) < maxContexts {
				v.Context = append(v.Context, c)
			}
		case "allow":
		case "rewrite":
			ev["text"], v.Action, v.Text, v.By = d.Text, "rewrite", d.Text, hf.Name
		default: // reply, silent, block
			v.Action, v.Text, v.Reason, v.By = d.Action, d.Text, d.Reason, hf.Name
			return v
		}
	}
	return v
}

// runHandler runs one handler for event and reads its decision, hooked as a tool call.
func (rt *Runtime) runHandler(ctx context.Context, hf HandlerFile, event, session string, args []byte) (d Decision, err error) {
	call := wire.ToolCall{ID: fmt.Sprintf("handler_%x", time.Now().UnixNano()), Name: HookPrefix + hf.Name + "#" + event,
		Arguments: fmt.Sprintf(`{"event":%q,"handler":%q}`, event, hf.File)}
	var obs HookObserver
	if rt.Observer != nil {
		obs = rt.Observer()
	}
	if obs != nil {
		obs.ToolStart(session, call)
	}
	started := time.Now()
	defer func() {
		if obs != nil {
			result := `{"action":"` + d.Action + `"}`
			if err != nil {
				result = `{"error":"handler failed"}`
			}
			obs.ToolDone(session, call, result, err != nil, time.Since(started))
		}
	}()
	res, c, err := rt.Exec(ctx, Exec{Name: hf.Name, Session: session, Source: hf.Source, Header: hf.Header, Args: args,
		Entry: event, Script: event == OnSchedule})
	if err != nil {
		return Decision{}, err
	}
	if res.Err != nil {
		msg := res.Err.Error()
		if logs := c.Logs(); len(logs) > 0 {
			msg += " (logs: " + clip(strings.Join(logs, " | "), 500) + ")"
		}
		return Decision{}, errors.New(msg)
	}
	return ParseDecision(event, res.Value)
}

// Events are the runtime's handlers as the gateway (messaging.Handlers) and the scheduler call
// them.
type Events struct{ RT *Runtime }

var _ messaging.Handlers = Events{}

// Inbound runs on_message or on_mention for a message the gateway answers.
func (e Events) Inbound(ctx context.Context, event string, in messaging.Inbound) Verdict {
	return e.RT.Dispatch(ctx, event, in.Session, asMap(in), nil)
}

// BeforeSend runs before_send for a message about to go out.
func (e Events) BeforeSend(ctx context.Context, out messaging.Outgoing) Verdict {
	return e.RT.Dispatch(ctx, BeforeSend, out.Session, asMap(out), nil)
}

// OnSchedule runs on_schedule for a job's run: ev is the event (job, script output, run context),
// and only the handlers whose jobs name the job (by id or name, or "*") run.
func (e Events) OnSchedule(ctx context.Context, id, name string, ev map[string]any) Verdict {
	return e.RT.Dispatch(ctx, OnSchedule, "cron-"+id, ev, func(h *Header) bool { return h.Serves(id, name) })
}

// asMap is v as the JSON object a handler gets.
func asMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}
