// Package tools is the runtime's built-in tools (docs/needletail.md §4.5). Names stay
// Hermes-compatible (terminal, read_file, write_file, patch, search_files, memory) so bench files,
// schedules' tool lists and the core's wake text keep working when an agent switches engines.
//
// Every result is a JSON object; a failure is {"error": "..."} so the model can read and recover.
// Results pass through the redactor before anyone (the model, a session file, a log) sees them.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
)

// Env is what a tool call may use.
type Env struct {
	Home    string
	Sandbox sandbox.Sandbox
	// CommandTimeout is the terminal's default bound.
	CommandTimeout time.Duration
	// Environ is the terminal's environment (the scrubbed process env).
	Environ func() []string
	Memory  *memory.Store
	// Redact masks secret values in results.
	Redact func(string) string
	// SharedWrite is told about each file tool write under /shared (the core's fs_note, M2).
	SharedWrite func(ctx context.Context, path string)
	// Session is the calling session's id.
	Session string
}

// Tool is one function the model may call.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Run         func(ctx context.Context, env *Env, args json.RawMessage) (any, error)
}

// Registry is a set of tools.
type Registry struct {
	byName map[string]Tool
}

// NewRegistry holds tools (later ones replace earlier ones of the same name).
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{byName: map[string]Tool{}}
	for _, t := range tools {
		r.byName[t.Name] = t
	}
	return r
}

// Builtins are the M1 built-in tools.
func Builtins() []Tool {
	return []Tool{terminalTool(), readFileTool(), writeFileTool(), patchTool(), searchFilesTool(), memoryTool()}
}

// Names is the tools' names, sorted.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Get is one tool.
func (r *Registry) Get(name string) (Tool, bool) { t, ok := r.byName[name]; return t, ok }

// Filter is the tools named in allow (all when allow is empty).
func (r *Registry) Filter(allow []string) []Tool {
	out := []Tool{}
	for _, n := range r.Names() {
		if len(allow) == 0 || slices.Contains(allow, n) {
			out = append(out, r.byName[n])
		}
	}
	return out
}

// Call runs one tool call and returns its result as the JSON text the model gets, redacted, and
// whether it failed.
func (r *Registry) Call(ctx context.Context, env *Env, name, args string) (string, bool) {
	t, ok := r.byName[name]
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
		}
	}
	failed := err != nil
	if failed {
		out = map[string]string{"error": err.Error()}
	}
	b, merr := json.Marshal(out)
	if merr != nil {
		b, failed = []byte(`{"error":"result could not be encoded"}`), true
	}
	s := string(b)
	if env != nil && env.Redact != nil {
		s = env.Redact(s)
	}
	return s, failed
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
