// Package luau runs agent-written Luau (docs/needletail.md §5): luau.wasm (Luau's VM and
// compiler, package wasm) under wazero, a fresh instance for every run, with the `nt` bindings
// and their global aliases (bindings.go) answered by the host behind capability checks
// (host.go). Tools are .luau files inside skills (tools.go); schedules may run .luau scripts.
//
// Two sandbox layers: WebAssembly's memory isolation (64 MiB, no files, no env, no args, a
// monotonic clock only) and Luau's own sandbox inside it (read-only globals and libraries, no
// load/getfenv/debug/require/io/os beyond os.time and os.clock, an instruction budget, a deadline
// and a memory cap). Only source is accepted; the shim compiles it, so bytecode never loads.
package luau

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/internal/needletail/luau/wasm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Bounds of one run.
const (
	MaxSource      = 1 << 20  // bytes of source
	MaxOutput      = 64 << 10 // bytes kept of stdout and of stderr each
	MaxHostIn      = 8 << 20  // bytes of one host call's arguments
	MaxHostOut     = 8 << 20  // bytes of one host call's answer
	MaxHostCalls   = 10_000   // host calls in one run
	DefaultTimeout = 30 * time.Second
	DefaultMemory  = 48 << 20 // the shim's soft cap (Luau's own allocations)
	DefaultBudget  = 2_000_000_000
	memoryPages    = 1024 // 64 MiB: the module's hard limit
)

// Host answers a run's host calls. name is a binding's name ("fs.read"), in its arguments as a
// JSON object; the answer is {"ok":true,"v":…} or {"ok":false,"err":{"code":…,"msg":…}}.
type Host interface {
	Call(ctx context.Context, name string, in []byte) []byte
}

// Request is one run.
type Request struct {
	// Name is the chunk's name in error messages (the tool's file).
	Name   string
	Source []byte
	// Entry is the function to call: "" calls the function the chunk returns (a tool's body),
	// a name calls that field of the table the chunk returns, else the global function the
	// script defined (an event handler). A chunk that returns no function returns its result.
	Entry string
	// Args is the entry's argument (and nt.args), JSON; nil is nil.
	Args json.RawMessage
	// Bindings are the bindings the script gets (nt.* and their global aliases).
	Bindings []Binding
	Host     Host
	Timeout  time.Duration // 0: DefaultTimeout
	Budget   int64         // interrupts (loop back edges and calls); 0: DefaultBudget
	Memory   int           // bytes; 0: DefaultMemory
	MaxCalls int           // host calls; 0: MaxHostCalls
}

// Result is a finished run: Value (JSON) when it succeeded, else Err.
type Result struct {
	// Untrusted is host provenance, never writable by the Luau program.
	Untrusted      string
	Value          json.RawMessage
	Err            *Error
	Stdout, Stderr string
	Took           time.Duration
}

// Error is how a run failed. Codes: syntax, runtime, timeout, budget, memory, entry, too_large,
// cancelled and trap (the module stopped: a fault the VM did not catch, such as a stack overflow).
type Error struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

// Runner compiles and runs Luau (the wasm engine; a native one could stand behind the same
// interface, §5.1).
type Runner interface {
	// Check compiles src and reports its syntax error, if any.
	Check(ctx context.Context, src []byte) error
	Run(ctx context.Context, req Request) (*Result, error)
}

// Engine is the wasm Runner: one wazero runtime and the compiled module, an instance per call.
// Safe for concurrent use.
type Engine struct {
	rt wazero.Runtime
	cm wazero.CompiledModule
}

var (
	sharedOnce   sync.Once
	sharedEngine *Engine
	sharedErr    error
)

// Shared is the process' engine, built on first use (compiling the module takes a moment).
func Shared() (*Engine, error) {
	sharedOnce.Do(func() { sharedEngine, sharedErr = NewEngine(context.Background()) })
	return sharedEngine, sharedErr
}

// NewEngine compiles luau.wasm. It runs on wazero's interpreter: its compiler (v1.12.0) mishandles
// the exception payload in large functions (Luau's parser catching its own error faults in
// __dynamic_cast), while the interpreter runs every error path correctly; it also needs no
// executable memory. TestParserErrorIsCaught keeps that case.
func NewEngine(ctx context.Context) (*Engine, error) {
	cfg := wazero.NewRuntimeConfigInterpreter().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
		WithMemoryLimitPages(memoryPages).
		WithCloseOnContextDone(true)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	fail := func(err error) (*Engine, error) {
		_ = rt.Close(ctx)
		return nil, err
	}
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return fail(err)
	}
	_, err := rt.NewHostModuleBuilder("env").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(hostCall),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("host_call").
		NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(hostResult),
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("host_result").
		Instantiate(ctx)
	if err != nil {
		return fail(err)
	}
	cm, err := rt.CompileModule(ctx, wasm.Module)
	if err != nil {
		return fail(err)
	}
	return &Engine{rt: rt, cm: cm}, nil
}

// Close releases the runtime.
func (e *Engine) Close(ctx context.Context) error { return e.rt.Close(ctx) }

// instance is one fresh module instance with its output streams.
type instance struct {
	mod            api.Module
	stdout, stderr *capped
	alloc, free    api.Function
}

func (e *Engine) instantiate(ctx context.Context) (*instance, error) {
	in := &instance{stdout: &capped{max: MaxOutput}, stderr: &capped{max: MaxOutput}}
	// No preopens, env or args; the monotonic clock is real (the shim's deadline), the wall clock
	// wazero's fixed fake (os.time asks the host instead).
	mc := wazero.NewModuleConfig().WithName("").WithStartFunctions("_initialize").
		WithSysNanotime().WithStdout(in.stdout).WithStderr(in.stderr)
	mod, err := e.rt.InstantiateModule(ctx, e.cm, mc)
	if err != nil {
		return nil, err
	}
	in.mod = mod
	in.alloc, in.free = mod.ExportedFunction("nt_alloc"), mod.ExportedFunction("nt_free")
	return in, nil
}

// put copies b into the instance's memory.
func (in *instance) put(ctx context.Context, b []byte) (uint32, error) {
	if len(b) == 0 {
		return 0, nil
	}
	res, err := in.alloc.Call(ctx, uint64(len(b)))
	if err != nil {
		return 0, err
	}
	p := uint32(res[0])
	if p == 0 || !in.mod.Memory().Write(p, b) {
		return 0, errors.New("luau: no memory for the input")
	}
	return p, nil
}

// out is the shim's last output.
func (in *instance) out(ctx context.Context) ([]byte, error) {
	p, err := in.mod.ExportedFunction("nt_out_ptr").Call(ctx)
	if err != nil {
		return nil, err
	}
	n, err := in.mod.ExportedFunction("nt_out_len").Call(ctx)
	if err != nil {
		return nil, err
	}
	b, ok := in.mod.Memory().Read(uint32(p[0]), uint32(n[0]))
	if !ok {
		return nil, errors.New("luau: output out of range")
	}
	return bytes.Clone(b), nil
}

// errNUL: source is text; a NUL byte (bytecode, or a binary) is refused before the compiler.
const errNUL = "source holds a NUL byte: only Luau source text is accepted"

// SyntaxError is a source the compiler refuses.
type SyntaxError struct{ Msg string }

func (e *SyntaxError) Error() string { return "syntax: " + e.Msg }

// Check compiles src in a fresh instance.
func (e *Engine) Check(ctx context.Context, src []byte) error {
	if len(src) > MaxSource {
		return &SyntaxError{Msg: fmt.Sprintf("source is %d bytes (at most %d)", len(src), MaxSource)}
	}
	if bytes.IndexByte(src, 0) >= 0 {
		return &SyntaxError{Msg: errNUL}
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	in, err := e.instantiate(ctx)
	if err != nil {
		return err
	}
	defer in.mod.Close(context.Background())
	p, err := in.put(ctx, src)
	if err != nil {
		return err
	}
	res, err := in.mod.ExportedFunction("nt_check").Call(ctx, uint64(p), uint64(len(src)))
	if err != nil {
		return fmt.Errorf("luau: check: %w", err)
	}
	if res[0] == 0 {
		return nil
	}
	msg, err := in.out(ctx)
	if err != nil {
		return err
	}
	return &SyntaxError{Msg: string(msg)}
}

// spec is nt_run's JSON argument.
type spec struct {
	Name     string          `json:"name"`
	Entry    string          `json:"entry,omitempty"`
	Args     json.RawMessage `json:"args,omitempty"`
	Bindings []wireBinding   `json:"bindings"`
}

type wireBinding struct {
	Name   string   `json:"name"`
	Global string   `json:"global,omitempty"`
	Params []string `json:"params,omitempty"`
	Spread bool     `json:"spread,omitempty"`
	Local  bool     `json:"local,omitempty"`
}

// Run runs req in a fresh instance. A script's failure is the Result's Err; the error is for a
// run that could not start.
func (e *Engine) Run(ctx context.Context, req Request) (*Result, error) {
	started := time.Now()
	res := &Result{}
	finish := func(in *instance) *Result {
		if in != nil {
			res.Stdout, res.Stderr = in.stdout.String(), in.stderr.String()
		}
		res.Took = time.Since(started)
		return res
	}
	if len(req.Source) > MaxSource {
		res.Err = &Error{Code: "too_large", Msg: fmt.Sprintf("source is %d bytes (at most %d)", len(req.Source), MaxSource)}
		return finish(nil), nil
	}
	if bytes.IndexByte(req.Source, 0) >= 0 {
		res.Err = &Error{Code: "syntax", Msg: errNUL}
		return finish(nil), nil
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	budget := req.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	mem := req.Memory
	if mem <= 0 {
		mem = DefaultMemory
	}
	sp := spec{Name: req.Name, Entry: req.Entry, Args: req.Args, Bindings: []wireBinding{}}
	if sp.Name == "" {
		sp.Name = "script"
	}
	if len(bytes.TrimSpace(sp.Args)) == 0 {
		sp.Args = nil
	} else if !json.Valid(sp.Args) {
		return nil, errors.New("luau: args are not valid JSON")
	}
	for _, b := range req.Bindings {
		sp.Bindings = append(sp.Bindings, b.wire())
	}
	specJSON, err := json.Marshal(sp)
	if err != nil {
		return nil, err
	}

	// The module's own deadline (the shim's interrupt) comes first, so the script gets a Luau
	// error it can report; the context closes the instance a moment later whatever it runs.
	runCtx, cancel := context.WithTimeout(ctx, timeout+500*time.Millisecond)
	defer cancel()
	st := &runState{host: req.Host, max: req.MaxCalls}
	if st.max <= 0 {
		st.max = MaxHostCalls
	}
	runCtx = context.WithValue(runCtx, stateKey{}, st)
	in, err := e.instantiate(runCtx)
	if err != nil {
		return nil, err
	}
	defer in.mod.Close(context.Background())
	srcP, err := in.put(runCtx, req.Source)
	if err != nil {
		return nil, err
	}
	specP, err := in.put(runCtx, specJSON)
	if err != nil {
		return nil, err
	}
	_, callErr := in.mod.ExportedFunction("nt_run").Call(runCtx, uint64(srcP), uint64(len(req.Source)),
		uint64(specP), uint64(len(specJSON)), uint64(mem), uint64(timeout.Milliseconds()), uint64(budget))
	if callErr != nil {
		switch {
		case ctx.Err() != nil:
			res.Err = &Error{Code: "cancelled", Msg: "the run was cancelled"}
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			res.Err = &Error{Code: "timeout", Msg: fmt.Sprintf("script timed out after %s", timeout)}
		default:
			msg := callErr.Error()
			if i := strings.IndexByte(msg, '\n'); i >= 0 {
				msg = msg[:i]
			}
			res.Err = &Error{Code: "trap", Msg: msg}
		}
		return finish(in), nil
	}
	out, err := in.out(runCtx)
	if err != nil {
		return nil, err
	}
	var ans struct {
		OK  bool            `json:"ok"`
		V   json.RawMessage `json:"v"`
		Err *Error          `json:"err"`
	}
	if err := json.Unmarshal(out, &ans); err != nil {
		res.Err = &Error{Code: "trap", Msg: "the module's answer is not JSON"}
		return finish(in), nil
	}
	if !ans.OK {
		res.Err = ans.Err
		if res.Err == nil {
			res.Err = &Error{Code: "runtime", Msg: "failed"}
		}
		return finish(in), nil
	}
	res.Value = ans.V
	return finish(in), nil
}

// ---- host calls ------------------------------------------------------------------------------

type stateKey struct{}

// runState is one run's host side: the pending answer of the last host_call (the shim fetches it
// right away with host_result) and the call count.
type runState struct {
	host    Host
	mu      sync.Mutex
	pending []byte
	has     bool
	calls   int
	max     int
}

// Handles are 1 (one answer is outstanding at a time); refusals are negative.
const (
	refusedNoRun    = -1
	refusedTooLarge = -2
	refusedName     = -3
	refusedTooMany  = -4
)

func failure(code, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"ok": false, "err": Error{Code: code, Msg: msg}})
	return b
}

// hostCall is env.host_call(name_ptr, name_len, in_ptr, in_len) -> handle.
func hostCall(ctx context.Context, m api.Module, stack []uint64) {
	st, _ := ctx.Value(stateKey{}).(*runState)
	namePtr, nameLen, inPtr, inLen := uint32(stack[0]), uint32(stack[1]), uint32(stack[2]), uint32(stack[3])
	ret := func(v int32) { stack[0] = api.EncodeI32(v) }
	if st == nil || st.host == nil {
		ret(refusedNoRun)
		return
	}
	if nameLen == 0 || nameLen > 64 {
		ret(refusedName)
		return
	}
	if inLen > MaxHostIn {
		ret(refusedTooLarge)
		return
	}
	name, ok := m.Memory().Read(namePtr, nameLen)
	if !ok {
		ret(refusedName)
		return
	}
	in, ok := m.Memory().Read(inPtr, inLen)
	if !ok {
		ret(refusedTooLarge)
		return
	}
	st.mu.Lock()
	st.calls++
	n := st.calls
	st.mu.Unlock()
	if n > st.max {
		ret(refusedTooMany)
		return
	}
	out := safeCall(ctx, st.host, string(name), bytes.Clone(in))
	if len(out) > MaxHostOut {
		out = failure("too_large", fmt.Sprintf("the answer is over %d bytes", MaxHostOut))
	}
	st.mu.Lock()
	st.pending, st.has = out, true
	st.mu.Unlock()
	ret(1)
}

// safeCall runs the host, turning a panic into a refusal: nothing unwinds into the module.
func safeCall(ctx context.Context, h Host, name string, in []byte) (out []byte) {
	defer func() {
		if p := recover(); p != nil {
			out = failure("host", fmt.Sprintf("%s failed", name))
		}
	}()
	out = h.Call(ctx, name, in)
	if !json.Valid(out) {
		out = failure("host", "bad answer")
	}
	return out
}

// hostResult is env.host_result(handle, out_ptr, cap) -> size: the pending answer's size, copied
// (and released) when it fits in cap.
func hostResult(ctx context.Context, m api.Module, stack []uint64) {
	st, _ := ctx.Value(stateKey{}).(*runState)
	handle, ptr, capacity := api.DecodeI32(stack[0]), uint32(stack[1]), uint32(stack[2])
	ret := func(v int32) { stack[0] = api.EncodeI32(v) }
	if st == nil || handle != 1 {
		ret(-1)
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.has {
		ret(-1)
		return
	}
	size := uint32(len(st.pending))
	if capacity < size || size == 0 {
		if size == 0 {
			st.pending, st.has = nil, false
		}
		ret(int32(size))
		return
	}
	if !m.Memory().Write(ptr, st.pending) {
		ret(-1)
		return
	}
	st.pending, st.has = nil, false
	ret(int32(size))
}

// capped keeps the first max bytes written to it.
type capped struct {
	mu  sync.Mutex
	max int
	buf bytes.Buffer
	cut bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := c.max - c.buf.Len(); room > 0 {
		c.buf.Write(p[:min(room, len(p))])
	}
	if len(p) > 0 && c.buf.Len() >= c.max {
		c.cut = true
	}
	return len(p), nil
}

func (c *capped) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := strings.ToValidUTF8(c.buf.String(), "�")
	if c.cut {
		s += "\n…[output truncated]"
	}
	return s
}
