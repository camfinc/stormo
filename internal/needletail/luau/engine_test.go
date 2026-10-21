package luau

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func engine(t *testing.T) *Engine {
	t.Helper()
	e, err := Shared()
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// fakeHost answers with fn, or echoes the call.
type fakeHost struct {
	mu    sync.Mutex
	calls []string
	fn    func(name string, in []byte) []byte
}

func (h *fakeHost) Call(_ context.Context, name string, in []byte) []byte {
	h.mu.Lock()
	h.calls = append(h.calls, name+" "+string(in))
	h.mu.Unlock()
	if h.fn != nil {
		return h.fn(name, in)
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "v": map[string]any{"name": name, "in": json.RawMessage(in)}})
	return b
}

func run(t *testing.T, src string, mod func(*Request)) *Result {
	t.Helper()
	req := Request{Name: "test.luau", Source: []byte(src), Bindings: Bindings(), Host: &fakeHost{}, Timeout: 30 * time.Second}
	if mod != nil {
		mod(&req)
	}
	res, err := engine(t).Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func wantValue(t *testing.T, res *Result, want string) {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("error %v (stdout %q)", res.Err, res.Stdout)
	}
	var got, w any
	if err := json.Unmarshal(res.Value, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("value %s, want %s", gb, wb)
	}
}

func wantErr(t *testing.T, res *Result, code, contains string) {
	t.Helper()
	if res.Err == nil {
		t.Fatalf("no error, value %s; want %s", res.Value, code)
	}
	if res.Err.Code != code || !strings.Contains(res.Err.Msg, contains) {
		t.Fatalf("error %v, want %s containing %q", res.Err, code, contains)
	}
}

func TestToolBodyGetsArgs(t *testing.T) {
	res := run(t, `return function(args) return {sum = args.a + args.b, same = args == nt.args, list = {1, 2, "x"}} end`,
		func(r *Request) { r.Args = json.RawMessage(`{"a": 2, "b": 3}`) })
	wantValue(t, res, `{"sum": 5, "same": true, "list": [1, 2, "x"]}`)
}

func TestChunkResultWithoutFunction(t *testing.T) {
	wantValue(t, run(t, `print("hi") return "plain"`, nil), `"plain"`)
	if res := run(t, `print("hi", 1)`, nil); res.Stdout != "hi\t1\n" || string(res.Value) != "null" {
		t.Fatalf("stdout %q value %s", res.Stdout, res.Value)
	}
}

// An event handler: a named function of the returned table, or a global the script defined,
// called with a JSON argument (M6b's on_message and friends run this way).
func TestEntry(t *testing.T) {
	wantValue(t, run(t, `return {on_message = function(ev) return {reply = "got " .. ev.text} end}`,
		func(r *Request) { r.Entry = "on_message"; r.Args = json.RawMessage(`{"text":"hi"}`) }), `{"reply": "got hi"}`)
	wantValue(t, run(t, `function on_schedule(ev) return ev.n * 2 end`,
		func(r *Request) { r.Entry = "on_schedule"; r.Args = json.RawMessage(`{"n":21}`) }), `42`)
	wantErr(t, run(t, `local x = 1`, func(r *Request) { r.Entry = "on_message" }), "entry", "on_message")
}

func TestCheck(t *testing.T) {
	e := engine(t)
	if err := e.Check(context.Background(), []byte(`return function(a: number): number return a end`)); err != nil {
		t.Fatalf("typed Luau refused: %v", err)
	}
	var se *SyntaxError
	if err := e.Check(context.Background(), []byte(`return function(`)); !errors.As(err, &se) {
		t.Fatalf("got %v, want a syntax error", err)
	}
}

// The parser raises its "recursion depth" error as a C++ exception from deep inside itself. On
// wazero's compiler engine (v1.12.0) catching it faults; the interpreter the engine is pinned to
// catches it, and the limit (200) keeps the parser inside the interpreter's call stack.
func TestParserErrorIsCaught(t *testing.T) {
	for _, src := range []string{
		"return " + strings.Repeat("(", 3000) + "1" + strings.Repeat(")", 3000),
		"return " + strings.Repeat("{", 3000) + strings.Repeat("}", 3000),
		strings.Repeat("do ", 3000) + strings.Repeat("end ", 3000),
	} {
		wantErr(t, run(t, src, nil), "syntax", "Exceeded allowed recursion depth")
		var se *SyntaxError
		if err := engine(t).Check(context.Background(), []byte(src)); !errors.As(err, &se) {
			t.Fatalf("check: %v", err)
		}
	}
}

func TestJSON(t *testing.T) {
	wantValue(t, run(t, `
		local v = nt.json.decode('{"a":[1,2,{"b":null,"c":"\\u00e9\\ud83d\\ude00"}],"t":true,"n":-1.5e2,"e":{}}')
		assert(v.a[3].b == nil and v.a[3].c == "é😀" and v.t == true and v.n == -150)
		local s = json_encode({x = nt.json.null, y = {}, z = {1, 2}})
		local back = json_decode(s)
		return {s = #s > 0, z = back.z, null = tostring(nt.json.null), enc = json_encode("a\"b\n")}`, nil),
		`{"s": true, "z": [1, 2], "null": "null", "enc": "\"a\\\"b\\n\""}`)
	wantErr(t, run(t, `local t = {} t.self = t return json_encode(t)`, nil), "runtime", "nested too deep")
	wantErr(t, run(t, `return json_encode(function() end)`, nil), "runtime", "cannot encode a function")
	wantErr(t, run(t, `return json_decode("{bad")`, nil), "runtime", "json:")
	wantErr(t, run(t, `return json_decode(string.rep("[", 100) .. string.rep("]", 100))`, nil), "runtime", "nested too deep")
	wantErr(t, run(t, `return 0/0`, nil), "runtime", "NaN")
}

func TestBindingsReachTheHost(t *testing.T) {
	h := &fakeHost{}
	res := run(t, `
		local a = nt.fs.read("x.md")
		local b = read_file("y.md")
		local c = http_request{url = "https://api.example.com", headers = {A = "1"}}
		local d = core_call("msg_send", {to = "atlas"})
		return {a.name, b["in"].path, c["in"].url, d["in"].tool, os.time() ~= nil}`, func(r *Request) {
		r.Host = h
		h.fn = func(name string, in []byte) []byte {
			if name == "time.now" {
				return []byte(`{"ok":true,"v":1700000000.5}`)
			}
			b, _ := json.Marshal(map[string]any{"ok": true, "v": map[string]any{"name": name, "in": json.RawMessage(in)}})
			return b
		}
	})
	wantValue(t, res, `["fs.read", "y.md", "https://api.example.com", "msg_send", true]`)
	want := []string{`fs.read {"path":"x.md"}`, `fs.read {"path":"y.md"}`}
	if !slices.Equal(h.calls[:2], want) {
		t.Fatalf("calls %q", h.calls)
	}
}

// A refusal is a Luau error the script can catch, "<code>: <msg>".
func TestHostRefusalIsAnError(t *testing.T) {
	h := &fakeHost{fn: func(string, []byte) []byte { return failure("denied", "nope") }}
	wantValue(t, run(t, `local ok, e = pcall(read_file, "x") return {ok, string.find(e, "denied: nope", 1, true) ~= nil}`,
		func(r *Request) { r.Host = h }), `[false, true]`)
	wantErr(t, run(t, `return read_file("x")`, func(r *Request) { r.Host = h }), "runtime", "denied: nope")
}

func TestGlobals(t *testing.T) {
	// Frozen: scripts agents write call these names (the tool format is a contract, §8).
	frozen := []string{"append_file", "core_call", "http_request", "json_decode", "json_encode", "list_files", "lock", "log", "now",
		"read_file", "read_memory", "remove_file", "send_message", "stat_file", "unlock", "write_file"}
	var got []string
	for _, b := range Bindings() {
		if b.Global != "" {
			got = append(got, b.Global)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, frozen) {
		t.Fatalf("globals %q, frozen %q", got, frozen)
	}
	// each is the same function as its nt binding
	var checks []string
	for _, b := range Bindings() {
		if b.Global != "" {
			checks = append(checks, "assert("+b.Global+" == nt."+b.Name+", '"+b.Global+"')")
		}
	}
	wantValue(t, run(t, strings.Join(checks, "\n")+"\nreturn true", nil), `true`)
}

func TestReference(t *testing.T) {
	ref := Reference()
	for _, b := range Bindings() {
		if !strings.Contains(ref, "nt."+b.Name) || b.Global != "" && !strings.Contains(ref, "`"+b.Global) || !strings.Contains(ref, b.Example) {
			t.Errorf("reference misses %s", b.Name)
		}
		if !b.Local && b.run == nil {
			t.Errorf("%s has no host side", b.Name)
		}
	}
}

// ---- adversarial: the VM ------------------------------------------------------------------

func TestInfiniteLoopTimesOut(t *testing.T) {
	for _, src := range []string{
		`while true do end`,
		`while true do pcall(function() while true do end end) end`,
		`for i = 1, math.huge do local s = tostring(i) end`,
	} {
		start := time.Now()
		res := run(t, src, func(r *Request) { r.Timeout = 300 * time.Millisecond })
		wantErr(t, res, "timeout", "timed out")
		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("%s: took %s", src, took)
		}
	}
}

func TestBudget(t *testing.T) {
	wantErr(t, run(t, `local n = 0 while true do n += 1 end`, func(r *Request) { r.Budget = 10_000 }), "budget", "instruction budget")
}

func TestDeepRecursion(t *testing.T) {
	wantErr(t, run(t, `local function f(n) return 1 + f(n + 1) end return f(1)`, nil), "runtime", "stack overflow")
	for _, src := range []string{
		`local mt = {} mt.__index = function(t, k) return t[k] end return setmetatable({}, mt).x`,
		`local mt = {} mt.__tostring = function(t) return tostring(t) end return tostring(setmetatable({}, mt))`,
		`local function g(s) return (string.gsub(s, ".", g)) end return g("abc")`,
		`local function c(a, b) table.sort({3, 2, 1}, c) return a < b end table.sort({3, 2, 1}, c)`,
	} {
		wantErr(t, run(t, src, nil), "runtime", "C stack overflow")
	}
}

func TestMemoryBomb(t *testing.T) {
	mem := func(r *Request) { r.Memory = 4 << 20 }
	for _, src := range []string{
		`local t = {} while true do t[#t + 1] = string.rep("x", 1e5) .. #t end`,
		`local t = {} for i = 1, 1e9 do t[i] = i end`,
		`local s = "x" while true do s = s .. s end`,
		`return buffer.create(1e9)`,
		`return string.rep("x", 2^31)`,
	} {
		res := run(t, src, mem)
		if res.Err == nil || res.Err.Code != "memory" && !strings.Contains(res.Err.Msg, "too large") {
			t.Fatalf("%s: %v", src, res.Err)
		}
	}
	// catchable, and the script goes on within its cap
	wantValue(t, run(t, `local ok, e = pcall(function() local t = {} while true do t[#t + 1] = string.rep("y", 1e5) .. #t end end)
		return {ok, e}`, mem), `[false, "not enough memory"]`)
}

func TestBytecodeIsRefused(t *testing.T) {
	// what luau_compile would give for `return 1`, and Lua 5.1's header: both are source here
	for _, src := range []string{"\x06\x03\x02\x01\x05", "\x1bLuaQ\x01\x04\x08", "\x06\x03\x00\x01\x00\x00\x00", "\x00return 1"} {
		wantErr(t, run(t, src, nil), "syntax", "")
	}
}

func TestEscapes(t *testing.T) {
	for _, src := range []string{
		`return getfenv(1)`, `return setfenv(1, {})`, `return loadstring("return 1")()`, `return load("return 1")()`,
		`return require("x")`, `return debug.traceback()`, `return newproxy(true)`, `return io.open("/etc/passwd")`,
		`return os.execute("id")`, `return os.getenv("HOME")`, `return os.date()`, `return string.dump(print)`,
		`return dofile("/etc/passwd")`, `return collectgarbage("collect")`, `return gcinfo()`,
		`string.rep = nil`, `nt.fs.read = print`, `nt.fs = {}`, `getmetatable("").__index = {}`, `math.pi = 3`,
		`setmetatable(nt, {})`, `rawset(nt.json, "encode", print)`, `table.insert(nt.fs, 1)`,
	} {
		if res := run(t, src, nil); res.Err == nil {
			t.Errorf("%s: no error (value %s)", src, res.Value)
		}
	}
	wantValue(t, run(t, `return {type(collectgarbage("count")), type(os.clock()), os.date == nil, io == nil, debug == nil}`, nil),
		`["number", "number", true, true, true]`)
}

func TestHugeOutput(t *testing.T) {
	res := run(t, `for i = 1, 200 do print(string.rep("x", 1000)) end return 1`, nil)
	if len(res.Stdout) > MaxOutput+100 || !strings.HasSuffix(res.Stdout, "[output truncated]") {
		t.Fatalf("stdout %d bytes", len(res.Stdout))
	}
	wantValue(t, res, `1`)
}

func TestMalformedHostAnswers(t *testing.T) {
	for _, answer := range []string{`not json`, `{"ok":true`, `[]`, `{"ok":false}`, ``} {
		h := &fakeHost{fn: func(string, []byte) []byte { return []byte(answer) }}
		if res := run(t, `return read_file("x")`, func(r *Request) { r.Host = h }); res.Err == nil {
			t.Errorf("answer %q: no error", answer)
		}
	}
	panics := &fakeHost{fn: func(string, []byte) []byte { panic("boom") }}
	wantErr(t, run(t, `return read_file("x")`, func(r *Request) { r.Host = panics }), "runtime", "host: fs.read failed")
	huge := &fakeHost{fn: func(string, []byte) []byte {
		b, _ := json.Marshal(map[string]any{"ok": true, "v": strings.Repeat("x", MaxHostOut)})
		return b
	}}
	wantErr(t, run(t, `return read_file("x")`, func(r *Request) { r.Host = huge }), "runtime", "too_large")
}

func TestHugeHostArguments(t *testing.T) {
	if raceEnabled {
		t.Skip("9 MiB through the interpreter under the race detector takes minutes")
	}
	wantErr(t, run(t, `return write_file("x", string.rep("x", 9 * 1024 * 1024))`, nil), "runtime", "refused")
}

func TestUnknownBinding(t *testing.T) {
	// a binding the host does not have (a spec the registry does not drive)
	h := &Call{rt: &Runtime{}}
	res := run(t, `return nt.nope.call()`, func(r *Request) {
		r.Bindings = append(Bindings(), Binding{Name: "nope.call"})
		r.Host = h
	})
	wantErr(t, res, "runtime", "unknown")
}

func TestTooManyHostCalls(t *testing.T) {
	wantErr(t, run(t, `for i = 1, 60 do now() end`, func(r *Request) { r.MaxCalls = 50 }), "runtime", "refused")
}

func TestConcurrentRuns(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := engine(t).Run(context.Background(), Request{Source: []byte(`return function(a) return a.n * 2 end`),
				Args: json.RawMessage(`{"n": 4}`), Host: &fakeHost{}})
			if err != nil || res.Err != nil || string(res.Value) != "8" {
				t.Errorf("run: %v %v %s", err, res.Err, res.Value)
			}
		}()
	}
	wg.Wait()
}

func TestCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	res, err := engine(t).Run(ctx, Request{Source: []byte(`while true do end`), Host: &fakeHost{}, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	wantErr(t, res, "cancelled", "")
}
