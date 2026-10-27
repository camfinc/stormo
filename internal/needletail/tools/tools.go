// Package tools is the runtime's built-in tools (docs/needletail.md §4.5). Names stay
// Hermes-compatible (terminal, read_file, write_file, patch, search_files, memory, skills_list,
// skill_view, skill_manage) so bench files,
// schedules' tool lists and the core's wake text keep working when an agent switches engines.
//
// Every result is a JSON object; a failure is {"error": "..."} so the model can read and recover.
// Results pass through the redactor before anyone (the model, a session file, a log) sees them.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/jail"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
	"github.com/camfinc/stormo/internal/needletail/skills"
)

// UntrustedWhy is why a call may defer no work ("" when it may): its run's context
// (messaging.UntrustedWhy), else env's Untrusted for its session.
func UntrustedWhy(ctx context.Context, env *Env) string {
	if messaging.Endorsed(ctx) != "" {
		return "" // an approved action: the approval endorses it, whatever the conversation's mark
	}
	if why := messaging.UntrustedWhy(ctx); why != "" {
		return why
	}
	if env != nil && env.Untrusted != nil {
		return env.Untrusted(ctx, env.Session)
	}
	return ""
}

// Env is what a tool call may use.
type Env struct {
	Home    string
	Sandbox sandbox.Sandbox
	// CommandTimeout is the terminal's default bound.
	CommandTimeout time.Duration
	// Environ is the terminal's environment (the scrubbed process env).
	Environ func() []string
	Memory  *memory.Store
	// Skills is the home's skills (the skill tools, and the terminal's SKILL_DIR).
	Skills *skills.Library
	// Redact masks secret values in results.
	Redact func(string) string
	// Flag is what the dream's scrubber (the instance's scrub rules) finds in text: what is
	// exported for the dream must have no finding. nil: no check beyond Redact.
	Flag func(string) []string
	// LongestSecret is the length of the longest text Redact masks: the terminal keeps that much
	// overlap where it cuts output, so a cut never leaves part of a secret (nil: 0).
	LongestSecret func() int
	// SharedWrite is told about each file tool write under /shared (the core's fs_note, M2).
	SharedWrite func(ctx context.Context, path string)
	// Session is the calling session's id.
	Session string
	// Untrusted, when set, says why a call in session may defer no work ("" when it may): the
	// run acts on words the agent may not obey, or its conversation has taken some in
	// (docs/needletail.md §7). nil: only the run's own context decides (messaging.UntrustedWhy).
	Untrusted func(ctx context.Context, session string) string
	// Touched, set per call by the loop, is told the /shared paths a tool wrote another way than
	// the file tools (a Luau tool through the core), so its tool_done hook names them.
	Touched func(path string)
	// Agent, when set, is the user the agent's commands run as (the terminal, a schedule's
	// scripts): the uid split (docs/needletail.md §7). nil: the runtime's own.
	Agent *User
	// Jail, when set, starts the agent's commands confined to its home and shared layers
	// (Landlock, docs/needletail.md §7). nil: they reach what the container and their user may.
	Jail *jail.Launcher
	// TmpDir, when set, is the agent's commands' temporary directory (TMPDIR, TMP and TEMP), in
	// the home: /tmp is outside the jail, and not writable with a read-only root filesystem.
	TmpDir string
	// Processes is the agent's background processes (terminal background: true and
	// process_manage); nil: none can start.
	Processes *Processes
	// Conversation is the conversation (its root session) a session belongs to, which owns the
	// background processes its turns start; nil or "": the session itself.
	Conversation func(session string) string
	// Approve puts a call a gate answered with messaging.ApprovalNeeded to a person (the
	// approvals service, docs/needletail.md §7): the tool's answer is then "pending approval",
	// and the runtime runs exactly that call if they approve. nil: such a call is refused.
	Approve func(ctx context.Context, env *Env, tool string, args json.RawMessage, need *messaging.ApprovalNeeded) (any, error)
	// Risky is the risky-command rule a terminal command matches ("" when none,
	// approvals.commands): it needs approval in any conversation. nil: none is risky.
	Risky func(command string) string
	// TaintedDrop are env names a tainted run's command never gets (the core's credentials,
	// secrets.Passthrough, and the Luau http_secrets).
	TaintedDrop []string
	// WorkDir is set on a tainted run's command env (quarantineEnv): its work directory, where
	// it starts and may write.
	WorkDir string
	tainted bool
}

// User is a uid and gid a command runs as.
type User struct{ UID, GID uint32 }

// Tool is one function the model may call.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Run         func(ctx context.Context, env *Env, args json.RawMessage) (any, error)
	// Hook is the name observers (the core's hooks, the bench) see; "" is Name. Luau tools are
	// luau:<name>.
	Hook string
	// HookArgs, when set, is the arguments as observers see them (the memory tools: metadata
	// only, never a memory's text); nil: as called.
	HookArgs func(args string) string
}

// ObservedName is the name observers see.
func (t Tool) ObservedName() string {
	if t.Hook != "" {
		return t.Hook
	}
	return t.Name
}

// Registry is a set of tools; safe for concurrent use (the core's tools join it after boot).
type Registry struct {
	mu     sync.RWMutex
	byName map[string]Tool
	groups map[string][]string // a group's names (SetGroup)
}

// NewRegistry holds tools (later ones replace earlier ones of the same name).
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{byName: map[string]Tool{}, groups: map[string][]string{}}
	for _, t := range tools {
		r.byName[t.Name] = t
	}
	return r
}

// Add registers tools, replacing any of the same name.
func (r *Registry) Add(tools ...Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range tools {
		r.byName[t.Name] = t
	}
}

// SetGroup replaces the tools of group (the Luau tools, found again every turn): the group's
// previous tools go, so a deleted or broken tool is no longer offered. A tool whose name a tool
// outside the group has is left out.
func (r *Registry) SetGroup(group string, tools []Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range r.groups[group] {
		delete(r.byName, n)
	}
	names := []string{}
	for _, t := range tools {
		if _, taken := r.byName[t.Name]; taken {
			continue
		}
		r.byName[t.Name] = t
		names = append(names, t.Name)
	}
	r.groups[group] = names
}

// Taken reports a name a tool outside group has.
func (r *Registry) Taken(group, name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.byName[name]; !ok {
		return false
	}
	return !slices.Contains(r.groups[group], name)
}

// Builtins are the built-in tools.
func Builtins() []Tool {
	return []Tool{terminalTool(), readFileTool(), writeFileTool(), patchTool(), searchFilesTool(), memoryTool(),
		memorySearchTool(), memoryManageTool(), skillsListTool(), skillViewTool(), skillManageTool(), processTool()}
}

// Names is the tools' names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.names()
}

func (r *Registry) names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Get is one tool.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.byName[name]
	return t, ok
}

// Filter is the tools named in allow (all when allow is empty).
func (r *Registry) Filter(allow []string) []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []Tool{}
	for _, n := range r.names() {
		if len(allow) == 0 || slices.Contains(allow, n) {
			out = append(out, r.byName[n])
		}
	}
	return out
}

// Call runs one tool call and returns its result as the JSON text the model gets, redacted, and
// whether it failed.
func (r *Registry) Call(ctx context.Context, env *Env, name, args string) (string, bool) {
	t, ok := r.Get(name)
	var out any
	var err error
	if !ok {
		err = fmt.Errorf("unknown tool %q (available: %v)", name, r.Names())
	} else {
		raw := json.RawMessage(args)
		if len(raw) == 0 {
			raw = json.RawMessage("{}")
		}
		if !json.Valid(raw) {
			err = fmt.Errorf("arguments are not valid JSON")
		} else {
			out, err = safeRun(ctx, t, env, raw)
			// a gate's "a person must approve this": put to the approvers, never run now; an
			// approved call (endorsed) that asks again is refused, never put twice
			if need, ok := messaging.AsApproval(err); ok && env != nil && env.Approve != nil && messaging.Endorsed(ctx) == "" {
				out, err = env.Approve(ctx, env, name, raw, need)
			}
		}
	}
	failed := err != nil
	if failed {
		out = map[string]string{"error": err.Error()}
	}
	redact := func(s string) string { return s }
	if env != nil && env.Redact != nil {
		redact = env.Redact
	}
	// Masked before encoding (a value JSON escapes differs once encoded) and after (for what the
	// walk cannot reach, such as raw JSON).
	if out != nil {
		out = redactValue(reflect.ValueOf(out), redact).Interface()
	}
	b, merr := json.Marshal(out)
	if merr != nil {
		b, failed = []byte(`{"error":"result could not be encoded"}`), true
	}
	return redact(string(b)), failed
}

// redactValue is a copy of v with redact applied to every string in it: struct fields, map
// values, slice and array elements, through pointers and interfaces. Byte slices are left alone.
func redactValue(v reflect.Value, redact func(string) string) reflect.Value {
	switch v.Kind() {
	case reflect.String:
		out := reflect.New(v.Type()).Elem()
		out.SetString(redact(v.String()))
		return out
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(redactValue(v.Elem(), redact))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(redactValue(v.Elem(), redact))
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < out.NumField(); i++ {
			if f := out.Field(i); f.CanSet() {
				f.Set(redactValue(v.Field(i), redact))
			}
		}
		return out
	case reflect.Slice:
		if v.IsNil() || v.Type().Elem().Kind() == reflect.Uint8 {
			return v
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(redactValue(v.Index(i), redact))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(redactValue(v.Index(i), redact))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), redactValue(it.Value(), redact))
		}
		return out
	}
	return v
}

// safeRun turns a tool's panic into an error: one bad call must not take the runtime down.
func safeRun(ctx context.Context, t Tool, env *Env, args json.RawMessage) (out any, err error) {
	defer func() {
		if p := recover(); p != nil {
			out, err = nil, fmt.Errorf("tool %s crashed: %v", t.Name, p)
		}
	}()
	return t.Run(ctx, env, args)
}

// decode reads args into v, refusing unknown fields' types but not unknown fields (models add
// harmless extras).
func decode(args json.RawMessage, v any) error {
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	return nil
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }
