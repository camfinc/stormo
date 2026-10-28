package luau

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/tools"
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

// skillFiles is every .luau file (regular, not a symlink; with all, any entry named .luau, which
// then fails to load) in a directory sub of the home's skills, as (skill dir, home path), sorted
// by skill dir, then file name. The directory is listed by descriptor (fsx.ReadDirBelow: no
// symlink below the home is followed), and each file is read the same way (readSkillFile).
func (rt *Runtime) skillFiles(sub string, all bool) [][2]string {
	var out [][2]string
	for _, s := range skills.Index(rt.Home) {
		ents, err := fsx.ReadDirBelow(rt.Home, path.Join("skills", s.Dir, sub))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".luau") || !all && !e.Type().IsRegular() {
				continue
			}
			out = append(out, [2]string{s.Dir, path.Join("skills", s.Dir, sub, e.Name())})
		}
	}
	// by skill dir, then file name: not by the whole path, where "ops/a-b/" comes before "ops/a/"
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return path.Base(out[i][1]) < path.Base(out[j][1])
	})
	return out
}

// readSkillFile reads a skill's .luau file (rel: its home path) by descriptor, at most MaxSource
// bytes: a FIFO, a device or a symlink a command swaps in is refused without blocking.
func (rt *Runtime) readSkillFile(rel string) ([]byte, error) {
	body, err := fsx.ReadFileBelow(rt.Home, rel, MaxSource)
	switch {
	case errors.Is(err, fsx.ErrTooBig):
		return nil, fmt.Errorf("over %d bytes", MaxSource)
	case errors.Is(err, fsx.ErrNotRegular), errors.Is(err, fsx.ErrLink):
		return nil, errors.New("not a regular file")
	}
	return body, err
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
	// Events are the events the file's header names, read leniently (liftEvents) when the file
	// fails to load: a before_send handler that cannot run blocks the message. None (a file
	// that cannot be read, or whose header names no events) is taken as every event.
	Events []string
}

// Handlers is every handler of the home's skills, valid or not (any entry named .luau in a
// skill's handlers/: a symlink, a FIFO or a directory is one that cannot be loaded), in the order
// they run: by skill dir, then file name. They are read again at every event, so a handler
// written in one conversation runs at the next event.
func (rt *Runtime) Handlers(ctx context.Context) []HandlerFile {
	var out []HandlerFile
	for _, f := range rt.skillFiles(HandlerDir, true) {
		hf := HandlerFile{Skill: f[0], File: f[1]}
		hf.Err = rt.loadHandler(ctx, &hf)
		out = append(out, hf)
	}
	return out
}

func (rt *Runtime) loadHandler(ctx context.Context, hf *HandlerFile) error {
	body, err := rt.readSkillFile(hf.File)
	if err != nil {
		return err
	}
	hf.Events = liftEvents(body)
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

// eventsLine is a header's events line in flow form, for a header YAML cannot read.
var eventsLine = regexp.MustCompile(`(?m)^events\s*:\s*\[([^\]\n]*)\]`)

// liftEvents is the events a handler file's header names, read as leniently as it can be: the
// header as a handler's, else its block as any YAML (unknown keys, bad caps or timeout, an
// unknown event beside known ones), else an `events: [...]` line. nil when none can be read.
func liftEvents(body []byte) []string {
	if h, err := ParseHeaderOf(body, KindHandler); err == nil {
		return h.Events
	}
	block := headerBlock(body)
	var raw struct {
		Events any `yaml:"events"`
	}
	var names []string
	if yaml.Unmarshal(block, &raw) == nil {
		switch v := raw.Events.(type) {
		case []any:
			for _, e := range v {
				names = append(names, fmt.Sprint(e))
			}
		case string:
			names = strings.Split(v, ",")
		}
	} else if m := eventsLine.FindSubmatch(block); m != nil {
		names = strings.Split(string(m[1]), ",")
	}
	var out []string
	for _, n := range names {
		if n = strings.Trim(strings.TrimSpace(n), `"'`); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// headerBlock is the lines between a header's opening line and its closing one (or the end of
// the file), at most maxHeader bytes; nil when the file does not open with a header.
func headerBlock(body []byte) []byte {
	body = bytes.TrimPrefix(body, []byte("\ufeff"))
	first, rest, _ := bytes.Cut(body, []byte("\n"))
	if strings.TrimRight(string(first), " \t\r") != headerOpen {
		return nil
	}
	var block []byte
	for len(rest) > 0 && len(block) <= maxHeader {
		line, more, _ := bytes.Cut(rest, []byte("\n"))
		if strings.TrimRight(string(line), " \t\r") == headerClose {
			break
		}
		block = append(append(block, line...), '\n')
		rest = more
	}
	return block
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
// send), Text a reply's or a rewrite's text, Context a pass's note for the model. Explicit: the
// handler named its action ("pass", {action = "pass"}); nil, nothing or a table with no action is
// the event's default, which is no pass for an ambient message (§4.9).
type Decision struct {
	Untrusted string `json:"-"`
	Action    string `json:"action"`
	Text      string `json:"text,omitempty"`
	Context   string `json:"context,omitempty"`
	Reason    string `json:"reason,omitempty"`
	// To and Note are a route's (on_message, on_mention): where the message goes and a note for
	// the recipient; Text is then an optional acknowledgement in the original conversation.
	To       string `json:"to,omitempty"`
	Note     string `json:"note,omitempty"`
	Explicit bool   `json:"-"`
}

// Bounds of a route's note and acknowledgement (characters).
const (
	MaxRouteNote = 1000
	MaxRouteAck  = 1000
)

// actions are each event's actions; the first is the default (what nil means).
var actions = map[string][]string{
	OnMessage:  {"pass", "reply", "silent", "route"},
	OnMention:  {"pass", "reply", "silent", "route"},
	OnSchedule: {"pass", "reply", "silent"},
	BeforeSend: {"allow", "rewrite", "block"},
}

// ParseDecision reads what a handler returned for event: nil (or nothing) is the default
// action; anything else is an object with an action of the event's (a string alone is taken as
// the action). Only a named action is Explicit.
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
		d.Action, d.Explicit = s, true
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
		if d.To, err = str("to"); err != nil {
			return Decision{}, err
		}
		if d.Note, err = str("note"); err != nil {
			return Decision{}, err
		}
		if d.Explicit = d.Action != ""; !d.Explicit {
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
	case "route":
		if strings.TrimSpace(d.To) == "" {
			return Decision{}, fmt.Errorf("route needs to (an agent id or slack:<target>)")
		}
		_, _, canonical, err := messaging.ParseRoute(d.To)
		if err != nil {
			return Decision{}, err
		}
		d.To = canonical
		if utf8.RuneCountInString(d.Note) > MaxRouteNote || utf8.RuneCountInString(d.Text) > MaxRouteAck {
			return Decision{}, fmt.Errorf("a route's note and acknowledgement (text) are at most %d characters each", MaxRouteNote)
		}
	}
	if d.Action != "route" {
		d.To, d.Note = "", ""
	}
	if d.Action != "reply" && d.Action != "rewrite" && d.Action != "route" {
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
// rewrite changes it and goes on, a block stops the message. A message's or a schedule's handler
// that fails, passes its bound or answers something that is not one of the event's actions is
// skipped: logged, hooked as failed, and the event goes on as if it had passed. before_send fails
// closed instead: such a handler, or one whose file declares before_send (read leniently) or
// names no events it could be read for (unreadable, over MaxSource, not a regular file, no
// header) but cannot be loaded, blocks the message (logged; hooked as failed when it ran), since the text it was there to
// change would otherwise go out as it was. serves, when set, picks the handlers that apply
// (on_schedule: the job's).
func (rt *Runtime) Dispatch(ctx context.Context, event, session string, ev map[string]any, serves func(*Header) bool) (v Verdict) {
	// an approval endorses the action a person saw, never the agent's handlers around it
	ctx = messaging.WithQuarantine(messaging.WithoutEndorsed(ctx), nil)
	defer func() { v.Untrusted = messaging.UntrustedWhy(ctx) }()
	v = Verdict{Action: actions[event][0]}
	if messaging.HandlerEvent(ctx) == BeforeSend && event == BeforeSend {
		return v // never reached (a before_send handler cannot send); no recursion either way
	}
	ctx = messaging.WithHandler(ctx, event)
	closed := event == BeforeSend
	for _, hf := range rt.Handlers(ctx) {
		if hf.Err != nil && closed && (len(hf.Events) == 0 || slices.Contains(hf.Events, event)) {
			return rt.failClosed(v, hf, event, "it cannot be loaded: "+hf.Err.Error())
		}
		if hf.Err != nil || !hf.Header.Handles(event) || serves != nil && !serves(hf.Header) {
			continue
		}
		ev["event"] = event
		args, err := json.Marshal(ev)
		if err != nil {
			if closed {
				return rt.failClosed(v, hf, event, "the event could not be encoded: "+err.Error())
			}
			rt.logf("handler %s (%s): the event could not be encoded: %v", hf.Name, event, err)
			return v
		}
		d, err := rt.runHandler(ctx, hf, event, session, args)
		v.Ran++
		if err != nil {
			if closed {
				return rt.failClosed(v, hf, event, err.Error())
			}
			rt.logf("handler %s (%s) skipped: %v", hf.Name, event, err)
			v.Skipped++
			continue
		}
		if d.Action == "route" {
			// a route needs the handler's route:<target> cap (declared ∩ granted), else it is
			// skipped as a failing handler is
			if caps, err := NewCaps(hf.Header.Caps, rt.Grants); err != nil || !caps.Route(d.To) {
				rt.logf("handler %s (%s) skipped: route to %s needs route:%s, which this handler does not have (its caps, as granted)", hf.Name, event, d.To, d.To)
				v.Skipped++
				continue
			}
		}
		switch d.Action {
		case "pass":
			if d.Explicit {
				v.Passed++
			}
			if c := strings.TrimSpace(d.Context); c != "" && len(v.Context) < maxContexts {
				v.Context = append(v.Context, c)
			}
		case "allow":
		case "rewrite":
			ev["text"], v.Action, v.Text, v.By = d.Text, "rewrite", d.Text, hf.Name
		default: // reply, silent, block, route
			v.Action, v.Text, v.Reason, v.By, v.To, v.Note = d.Action, d.Text, d.Reason, hf.Name, d.To, d.Note
			return v
		}
	}
	return v
}

// failClosed is a before_send verdict when handler hf could not decide: the message is blocked
// (logged), by hf, with why in the reason (clipped).
func (rt *Runtime) failClosed(v Verdict, hf HandlerFile, event, why string) Verdict {
	name := hf.Name
	if name == "" {
		if n, err := toolName(hf.Skill, strings.TrimSuffix(path.Base(hf.File), ".luau")); err == nil {
			name = n
		} else {
			name = hf.File
		}
	}
	rt.logf("handler %s (%s) failed: the message is blocked (before_send fails closed): %s", name, event, why)
	v.Action, v.Text, v.By, v.Failed = "block", "", name, true
	v.Reason = "the handler failed, so the message is blocked: " + clip(why, 300)
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
	d, err = ParseDecision(event, res.Value)
	d.Untrusted = res.Untrusted
	return d, err
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

// EventDoc is one event as the reference and the system prompt describe it.
type EventDoc struct {
	Event, When, Fields, Actions string
}

// EventDocs are the events, in EventNames' order.
var EventDocs = []EventDoc{
	{OnMessage, "a Slack message the agent answers (a DM, or a reply in a thread it is in), before a turn is taken for it; only messages from allowed_users reach it. " +
		"With slack.listen, also every other message in a channel the agent listens to (ambient: true): then nothing happens unless a handler answers an explicit pass (\"pass\" or {action = \"pass\"}; nil is no pass there) for a turn that may stay silent, or replies; " +
		"from a person allowed_users does not list (verified: false) only silent or pass, never a reply",
		"text, user, user_name, bot, verified, ambient, channel, thread_ts, ts, dm, mentioned, reply, session, thread_context[] {name, text, assistant, parent, unverified}",
		"pass (context?: a note for the model), reply {text} (posted here, no turn), silent (nothing posted), " +
			"route {to, note?, text?} (no turn: to an agent id or slack:<target>, with who wrote it; cap route:<to>; unverified: the owner approves first; text: an acknowledgement)"},
	{OnMention, "the same, for a message that mentions the agent (instead of on_message)", "as on_message", "as on_message"},
	{OnSchedule, "a scheduled job's run, after its monitor and pre-run script, before its turn; only for the jobs the header's jobs: names (id, name or \"*\")",
		"job {id, name, schedule, prompt, skills, deliver, repo}, script_output, run_context, time",
		"pass (context?: notes in the run's prompt), reply {text} (the run's output, no turn), silent (a silent run)"},
	{BeforeSend, "every message the agent posts (kind: reply, handler_reply, send, delivery, notice); the messaging policy still decides after it",
		"text, target, channel, thread_ts, kind, session, broadcast",
		"allow, rewrite {text} (the text only; never the target, never a broadcast), block {reason?}"},
}

// HandlerReference is the event handlers' part of the reference (markdown).
func HandlerReference() string {
	var b strings.Builder
	b.WriteString("## Event handlers\n\nA handler is `skills/<skill>/handlers/<name>.luau`: the tool header with `events: [...]` (and, with " +
		"on_schedule, `jobs: [...]`) instead of `args`, then a function per event, named after it (a global, or a field of the table " +
		"the chunk returns), called with the event and returning a decision: nil (the default), the action as a string, or " +
		"`{action = …, text = …, context = …, reason = …}`. Return `\"pass\"` explicitly to hand a message to the model: nil (or a " +
		"table with no action) passes a message addressed to you, but is no pass for an ambient one. Handlers of an event run in order (skill dir, then name) with their " +
		"effective caps and the Luau bound; a message's or a schedule's stop at the first reply or silent, before_send's pass the " +
		"text on through each rewrite and stop at a block. A message's or schedule's handler that fails, times out or answers no " +
		"valid action is skipped (logged) and the event goes on as if it had passed; a before_send one that fails (or cannot be " +
		"loaded) blocks the message (fails closed). send_message works in a handler (within its caps; the message meets " +
		"before_send), except in before_send. luau_test dry-runs a handler with a sample event.\n\n")
	for _, d := range EventDocs {
		fmt.Fprintf(&b, "### %s\n\nWhen: %s.\n\n- event: %s\n- answers: %s\n\n", d.Event, d.When, d.Fields, d.Actions)
	}
	return b.String()
}

// ReferenceTool is luau_reference: the full reference of the functions Luau tools and handlers
// get, and of the events (the system prompt carries the compact form).
func ReferenceTool() tools.Tool {
	return tools.Tool{Name: "luau_reference",
		Description: "The full reference for writing Luau tools and event handlers: every function (arguments, result, grant, an example) " +
			"and every event (its fields and the answers it takes).",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		Run: func(context.Context, *tools.Env, json.RawMessage) (any, error) {
			return map[string]any{"reference": Reference()}, nil
		}}
}
