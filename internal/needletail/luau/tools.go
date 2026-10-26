package luau

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/skills"
	"github.com/camfinc/stormo/internal/needletail/tools"
)

// ToolDir is where a skill keeps its Luau tools: skills/<skill>/tools/<name>.luau.
const ToolDir = "tools"

// HookPrefix names a Luau tool's calls in the hooks and the bench (luau:<tool>), apart from the
// built-in and core tools.
const HookPrefix = "luau:"

// Exec is one run of a script.
type Exec struct {
	// Name is the tool's registered name or the script's path (audit, errors).
	Name    string
	Session string
	Source  []byte
	// Header is the script's header (nil: no capabilities).
	Header *Header
	Args   json.RawMessage
	Entry  string
	DryRun bool
	// Script: a schedule's script (bounded by the script timeout, not the tools').
	Script bool
}

// Exec runs x with its effective capabilities (declared ∩ granted). Value, output and error
// messages are redacted.
func (rt *Runtime) Exec(ctx context.Context, x Exec) (*Result, *Call, error) {
	var declared []string
	if x.Header != nil {
		declared = x.Header.Caps
	}
	caps, err := NewCaps(declared, rt.Grants)
	if err != nil {
		return nil, nil, err
	}
	call := rt.NewCall(x.Name, x.Session, caps, x.DryRun)
	ctx = call.runContext(ctx)
	if why := call.quarantined(ctx); why != "" {
		if err := messaging.MarkUntrusted(ctx, why); err != nil {
			return nil, call, err
		}
	}
	res, err := rt.Runner.Run(ctx, Request{Name: x.Name, Source: x.Source, Entry: x.Entry, Args: x.Args, Bindings: Bindings(),
		Host: call, Timeout: rt.timeout(x.Header, x.Script)})
	if err != nil {
		return nil, call, err
	}
	res.Untrusted = messaging.UntrustedWhy(ctx)
	if res.Untrusted != "" {
		if err := messaging.MarkUntrusted(ctx, res.Untrusted); err != nil {
			return nil, call, err
		}
	}
	if len(res.Value) > 0 {
		res.Value = json.RawMessage(rt.redact(string(res.Value)))
	}
	res.Stdout, res.Stderr = rt.redact(res.Stdout), rt.redact(res.Stderr)
	if res.Err != nil {
		res.Err.Msg = rt.redact(res.Err.Msg)
	}
	return res, call, nil
}

// ToolFile is one .luau file of a skill's tools/: valid (Err nil) or not.
type ToolFile struct {
	// Skill is the skill's dir (crm/crm-api); File the tool's path in the home
	// (skills/crm/crm-api/tools/lookup.luau).
	Skill, File string
	// Name is the registered name, <skill>__<name> (the skill's last dir segment).
	Name   string
	Header *Header
	Source []byte
	Err    error
}

var toolFile = regexp.MustCompile(`^tools/([a-z][a-z0-9_]{0,47})\.luau$`)

// toolName is the registry name of a skill dir's tool: the dir's last segment, then the tool's
// name (models accept [A-Za-z0-9_-], at most 64 characters).
func toolName(skillDir, name string) (string, error) {
	n := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, path.Base(skillDir)) + "__" + name
	if len(n) > 64 {
		return "", fmt.Errorf("the tool's name %s is over 64 characters: shorten the skill's dir or the tool's name", n)
	}
	return n, nil
}

// CheckTool validates a skill's .luau file as skill_manage would leave it: rel is its path in the
// skill (tools/<name>.luau), body the whole file. The header must be valid and name the file, and
// the source must compile.
func (rt *Runtime) CheckTool(ctx context.Context, rel string, body []byte) error {
	m := toolFile.FindStringSubmatch(rel)
	if m == nil {
		return fmt.Errorf("%s: a Luau tool lives at tools/<name>.luau (name: a lowercase letter, then lowercase letters, digits or _)", rel)
	}
	h, err := ParseHeader(body, true)
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

// compiles checks src once per content (tools are discovered every turn). A syntax error is kept
// as long as the content is; a failure of the engine itself (the check timed out, say) is kept
// with its reason for checkFailureTTL, so a source that makes the check fail is not checked again
// every turn, and a transient failure still clears. A check the caller cancelled is not kept.
func (rt *Runtime) compiles(ctx context.Context, src []byte) error {
	sum := sha256.Sum256(src)
	now := time.Now()
	checked.mu.Lock()
	c, ok := checked.m[sum]
	checked.mu.Unlock()
	if ok && (c.until.IsZero() || now.Before(c.until)) {
		return c.err
	}
	err := rt.Runner.Check(ctx, src)
	var se *SyntaxError
	entry := checkResult{err: err}
	if err != nil && !errors.As(err, &se) {
		if ctx.Err() != nil {
			return err // the caller gave up: not kept
		}
		entry = checkResult{err: fmt.Errorf("the compile check failed (not retried for %s unless the file changes): %w", checkFailureTTL, err),
			until: now.Add(checkFailureTTL)}
	}
	checked.mu.Lock()
	if len(checked.m) > 1024 {
		checked.m = map[[32]byte]checkResult{}
	}
	checked.m[sum] = entry
	checked.mu.Unlock()
	return entry.err
}

// checkFailureTTL is how long a failure of the engine to check a source is kept.
const checkFailureTTL = 10 * time.Minute

type checkResult struct {
	err   error
	until time.Time // zero: kept while the content is
}

var checked = struct {
	mu sync.Mutex
	m  map[[32]byte]checkResult
}{m: map[[32]byte]checkResult{}}

// Discover is every .luau file in the home's skills' tools/ directories, in a stable order.
// Symlinks are skipped; a tool whose registered name another one (or a built-in) has is invalid.
func (rt *Runtime) Discover(ctx context.Context, reserved func(name string) bool) []ToolFile {
	var out []ToolFile
	for _, f := range rt.skillFiles(ToolDir, false) {
		tf := ToolFile{Skill: f[0], File: f[1]}
		tf.Err = rt.loadTool(ctx, &tf)
		out = append(out, tf)
	}
	seen := map[string]string{}
	for i := range out {
		tf := &out[i]
		if tf.Err != nil {
			continue
		}
		switch {
		case reserved != nil && reserved(tf.Name):
			tf.Err = fmt.Errorf("%s is the name of a built-in or core tool", tf.Name)
		case seen[tf.Name] != "":
			tf.Err = fmt.Errorf("%s is also the name of %s", tf.Name, seen[tf.Name])
		default:
			seen[tf.Name] = tf.File
		}
	}
	return out
}

func (rt *Runtime) loadTool(ctx context.Context, tf *ToolFile) error {
	body, err := rt.readSkillFile(tf.File)
	if err != nil {
		return err
	}
	if err := rt.CheckTool(ctx, ToolDir+"/"+path.Base(tf.File), body); err != nil {
		return err
	}
	h, _ := ParseHeader(body, true)
	name, err := toolName(tf.Skill, h.Name)
	if err != nil {
		return err
	}
	tf.Name, tf.Header, tf.Source = name, h, body
	return nil
}

// Status is the skill's tools as skill_view shows them.
func (rt *Runtime) Status(ctx context.Context, skillDir string, reserved func(string) bool) []skills.ToolStatus {
	out := []skills.ToolStatus{}
	for _, tf := range rt.Discover(ctx, reserved) {
		if tf.Skill != skillDir {
			continue
		}
		st := skills.ToolStatus{File: strings.TrimPrefix(tf.File, "skills/"+skillDir+"/"), Name: tf.Name}
		if tf.Err != nil {
			st.Error = tf.Err.Error()
		} else {
			caps, _ := NewCaps(tf.Header.Caps, rt.Grants)
			st.Caps, st.Effective = tf.Header.Caps, caps.Effective()
		}
		out = append(out, st)
	}
	return out
}

// Tools are the home's valid Luau tools as registry entries (the loop asks for them every turn).
func (rt *Runtime) Tools(ctx context.Context, reserved func(string) bool) []tools.Tool {
	var out []tools.Tool
	for _, tf := range rt.Discover(ctx, reserved) {
		if tf.Err != nil {
			continue
		}
		out = append(out, rt.tool(tf))
	}
	return out
}

func (rt *Runtime) tool(tf ToolFile) tools.Tool {
	desc := tf.Header.Description
	if len(tf.Header.Caps) > 0 {
		desc += " [Luau tool " + tf.File + "; caps: " + strings.Join(tf.Header.Caps, ", ") + "]"
	} else {
		desc += " [Luau tool " + tf.File + "]"
	}
	return tools.Tool{Name: tf.Name, Description: desc, Schema: tf.Header.Args, Hook: HookPrefix + tf.Name,
		Run: func(ctx context.Context, env *tools.Env, args json.RawMessage) (any, error) {
			res, call, err := rt.Exec(ctx, Exec{Name: tf.Name, Session: env.Session, Source: tf.Source, Header: tf.Header, Args: args})
			if call != nil && env.Touched != nil {
				for _, p := range call.Touched() {
					env.Touched(p)
				}
			}
			if err != nil {
				return nil, err
			}
			return toolResult(res, call, false)
		}}
}

// toolResult is what the model gets from a run: its result, its logs and output.
func toolResult(res *Result, call *Call, dry bool) (any, error) {
	out := map[string]any{}
	if res.Untrusted != "" {
		out["untrusted"] = true
	}
	if res.Value != nil {
		out["result"] = res.Value
	}
	if logs := call.Logs(); len(logs) > 0 {
		out["logs"] = logs
	}
	if res.Stdout != "" {
		out["stdout"] = res.Stdout
	}
	if res.Stderr != "" {
		out["stderr"] = res.Stderr
	}
	if res.Err != nil {
		if dry {
			out["error"] = res.Err.Error()
			return out, nil
		}
		msg := res.Err.Error()
		if logs := call.Logs(); len(logs) > 0 {
			msg += " (logs: " + strings.Join(logs, " | ") + ")"
		}
		if res.Stdout != "" {
			msg += " (stdout: " + clip(res.Stdout, 2000) + ")"
		}
		return nil, errors.New(msg)
	}
	return out, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestTool is luau_test: a dry run of a tool, by registered name or file, or of an event handler
// with a sample event, with writes, http, core calls and sends refused, so the agent can iterate
// on either before it is used.
func (rt *Runtime) TestTool(reserved func(string) bool) tools.Tool {
	return tools.Tool{
		Name: "luau_test",
		Description: "Dry-run a Luau tool (skills/<skill>/tools/<name>.luau) with test arguments, or an event handler " +
			"(skills/<skill>/handlers/<name>.luau) with a sample event: it runs with its effective capabilities, " +
			"but writes, http, core calls and sends are refused (code dry_run; a /shared listing or stat reads the mount instead of the core). " +
			"Shows the result or error, logs, output, refused calls and the caps; for a handler, the decision the runtime would act on. " +
			"Use it after writing a tool or a handler with skill_manage, before relying on it.",
		Schema: json.RawMessage(`{"type":"object","properties":{` +
			`"tool":{"type":"string","description":"a tool's registered name (<skill>__<name>) or file (skills/<skill>/tools/<name>.luau), or a handler's file (skills/<skill>/handlers/<name>.luau) or name (<skill>__<name>)"},` +
			`"event":{"type":"string","enum":["on_message","on_mention","on_schedule","before_send"],"description":"for a handler: the event to send it (default: the first it handles)"},` +
			`"args":{"type":"object","description":"a tool's arguments; for a handler, the event's fields (a sample event fills what you leave out)"}},"required":["tool"]}`),
		Run: func(ctx context.Context, env *tools.Env, raw json.RawMessage) (any, error) {
			var a struct {
				Tool  string          `json:"tool"`
				Event string          `json:"event"`
				Args  json.RawMessage `json:"args"`
			}
			if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Tool) == "" {
				return nil, errors.New("luau_test needs tool (a name or a file) and args")
			}
			want := strings.TrimPrefix(strings.TrimSpace(a.Tool), "./")
			match := func(name, file string) bool {
				return name == want || file == want || file == "skills/"+want || strings.TrimPrefix(file, "skills/") == want
			}
			isHandler := strings.Contains("/"+want, "/"+HandlerDir+"/") || a.Event != ""
			if !isHandler {
				var found *ToolFile
				all := rt.Discover(ctx, reserved)
				for i, tf := range all {
					if match(tf.Name, tf.File) {
						found = &all[i]
						break
					}
				}
				if found != nil {
					return rt.testTool(ctx, env, found, a.Args)
				}
			}
			for _, hf := range rt.Handlers(ctx) {
				if match(hf.Name, hf.File) {
					return rt.testHandler(ctx, env, hf, a.Event, a.Args)
				}
			}
			return nil, fmt.Errorf("no Luau tool or handler %q (a registered name <skill>__<name>, skills/<skill>/tools/<name>.luau or skills/<skill>/handlers/<name>.luau)", a.Tool)
		},
	}
}

func (rt *Runtime) testTool(ctx context.Context, env *tools.Env, found *ToolFile, args json.RawMessage) (any, error) {
	if found.Err != nil {
		return map[string]any{"tool": found.File, "valid": false, "error": found.Err.Error()}, nil
	}
	started := time.Now()
	res, call, err := rt.Exec(ctx, Exec{Name: found.Name, Session: env.Session, Source: found.Source, Header: found.Header, Args: args, DryRun: true})
	if err != nil {
		return nil, err
	}
	out, _ := toolResult(res, call, true)
	m := out.(map[string]any)
	caps, _ := NewCaps(found.Header.Caps, rt.Grants)
	m["tool"], m["valid"], m["caps"], m["effective_caps"] = found.Name, true, found.Header.Caps, caps.Effective()
	m["ok"] = res.Err == nil
	if d := call.Denied(); len(d) > 0 {
		m["denied"] = d
	}
	m["took_ms"] = time.Since(started).Milliseconds()
	return m, nil
}

// testHandler dry-runs a handler for event (its first by default) with args laid over the
// event's sample, and says what the runtime would do with its answer.
func (rt *Runtime) testHandler(ctx context.Context, env *tools.Env, hf HandlerFile, event string, args json.RawMessage) (any, error) {
	if hf.Err != nil {
		return map[string]any{"handler": hf.File, "valid": false, "error": hf.Err.Error()}, nil
	}
	if event == "" {
		event = hf.Header.Events[0]
	}
	if !hf.Header.Handles(event) {
		return nil, fmt.Errorf("%s does not handle %s (its events: %s)", hf.File, event, strings.Join(hf.Header.Events, ", "))
	}
	ev := SampleEvent(event)
	if len(args) > 0 && string(args) != "null" {
		var over map[string]any
		if err := json.Unmarshal(args, &over); err != nil {
			return nil, fmt.Errorf("args must be an object (the event's fields): %v", err)
		}
		for k, v := range over {
			ev[k] = v
		}
	}
	ev["event"] = event
	in, _ := json.Marshal(ev)
	started := time.Now()
	res, call, err := rt.Exec(ctx, Exec{Name: hf.Name, Session: env.Session, Source: hf.Source, Header: hf.Header, Args: in,
		Entry: event, DryRun: true, Script: event == OnSchedule})
	if err != nil {
		return nil, err
	}
	out, _ := toolResult(res, call, true)
	m := out.(map[string]any)
	caps, _ := NewCaps(hf.Header.Caps, rt.Grants)
	m["handler"], m["valid"], m["event"], m["caps"], m["effective_caps"] = hf.Name, true, event, hf.Header.Caps, caps.Effective()
	m["sent_event"] = json.RawMessage(in)
	m["ok"] = res.Err == nil
	if res.Err == nil {
		d, derr := ParseDecision(event, res.Value)
		if derr != nil {
			m["ok"], m["error"] = false, "the answer is not a decision: "+derr.Error()+" (the runtime would skip this handler)"
		} else {
			m["decision"] = d
		}
	} else {
		m["note"] = "the runtime would skip this handler (logged) and go on as if it passed"
	}
	if d := call.Denied(); len(d) > 0 {
		m["denied"] = d
	}
	m["took_ms"] = time.Since(started).Milliseconds()
	return m, nil
}

// SampleEvent is a made-up event of each kind, for luau_test and the reference.
func SampleEvent(event string) map[string]any {
	switch event {
	case OnMessage, OnMention:
		return map[string]any{"platform": "slack", "session": "slack-C0123456789-1700000000.000100", "channel": "C0123456789",
			"thread_ts": "1700000000.000100", "ts": "1700000000.000200", "dm": false, "mentioned": event == OnMention, "reply": true,
			"user": "U0123456789", "user_name": "Ana", "bot": false, "verified": true, "ambient": false, "text": "what is the status of the report?"}
	case OnSchedule:
		return map[string]any{"job": map[string]any{"id": "a1b2c3d4e5f6", "name": "daily report", "schedule": "every day at 09:00",
			"prompt": "Write the daily report.", "skills": []any{}, "deliver": "origin", "repo": false},
			"script_output": "", "run_context": "", "time": "2026-01-01T09:00:00Z"}
	case BeforeSend:
		return map[string]any{"platform": "slack", "target": "slack:C0123456789:1700000000.000100", "channel": "C0123456789",
			"thread_ts": "1700000000.000100", "kind": "reply", "session": "slack-C0123456789-1700000000.000100",
			"text": "The report is ready.", "broadcast": false}
	}
	return map[string]any{}
}

// RunScript runs a schedule's .luau script (a job's script or monitor): its header is required
// (format 1 and the caps it uses; name and description are optional), its bound the script
// timeout (or the header's, when lower). The output is what it printed, then what it returned (a
// string as it is, anything else as JSON); a failure is the error and the output.
func (rt *Runtime) RunScript(ctx context.Context, real, name string) (bool, string) {
	// real is resolved (no symlink in it): read by descriptor from the filesystem's root, every
	// component with O_NOFOLLOW, so nothing a command swaps in since (a FIFO, a link to a device
	// or to a file outside scripts/) is read
	body, err := fsx.ReadFileBelow("/", strings.TrimPrefix(filepath.Clean(real), "/"), MaxSource)
	switch {
	case errors.Is(err, fsx.ErrTooBig):
		return false, fmt.Sprintf("script %s is over %d bytes", name, MaxSource)
	case errors.Is(err, fsx.ErrNotRegular), errors.Is(err, fsx.ErrLink):
		return false, fmt.Sprintf("script %s is not a regular file", name)
	case err != nil:
		return false, fmt.Sprintf("script %s: %v", name, errors.Unwrap(err))
	}
	h, err := ParseHeader(body, false)
	if err != nil {
		return false, fmt.Sprintf("script %s: %v", name, err)
	}
	res, call, err := rt.Exec(ctx, Exec{Name: name, Session: "schedule", Source: body, Header: h, Script: true})
	if err != nil {
		return false, fmt.Sprintf("script %s: %v", name, err)
	}
	parts := []string{}
	if s := strings.TrimSpace(res.Stdout); s != "" {
		parts = append(parts, s)
	}
	if res.Err == nil && len(res.Value) > 0 && string(res.Value) != "null" {
		var s string
		if json.Unmarshal(res.Value, &s) == nil {
			parts = append(parts, s)
		} else {
			parts = append(parts, string(res.Value))
		}
	}
	out := strings.TrimSpace(strings.Join(parts, "\n"))
	if res.Err != nil {
		msg := "Script failed: " + res.Err.Error()
		if logs := call.Logs(); len(logs) > 0 {
			msg += "\nlogs:\n" + strings.Join(logs, "\n")
		}
		if out != "" {
			msg += "\nstdout:\n" + out
		}
		return false, msg
	}
	return true, out
}
