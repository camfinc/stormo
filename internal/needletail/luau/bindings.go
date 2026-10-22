package luau

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Binding is one function scripts get: nt.<Name> (nt.fs.read for "fs.read") and, when Global is
// set, the same function as a plain global (read_file). The registry (Bindings) is the one source
// of the module's nt table and globals (the shim builds them from it), the host's dispatch and
// the generated reference (Reference).
type Binding struct {
	// Name is the dotted path under nt and the host_call name ("fs.read").
	Name string
	// Global is the global alias ("read_file"), "" for none.
	Global string
	// Params are the positional arguments, sent to the host as an object keyed by their names.
	Params []Param
	// Spread: the one argument is a table sent as the whole argument object (http.request{…}).
	Spread bool
	// Returns describes the result.
	Returns string
	Doc     string
	// Grant is the capability the call needs, as the grammar writes it ("" for none).
	Grant   string
	Example string
	// Local bindings run inside the module (json); the host never sees them.
	Local bool
	// Write: the call changes something or reaches out (refused by luau_test's dry run).
	Write bool

	run func(ctx context.Context, c *Call, in json.RawMessage) (any, error)
}

// Param is one argument of a binding.
type Param struct {
	Name, Type, Doc string
	Optional        bool
}

func (b Binding) wire() wireBinding {
	w := wireBinding{Name: b.Name, Global: b.Global, Spread: b.Spread, Local: b.Local}
	for _, p := range b.Params {
		w.Params = append(w.Params, p.Name)
	}
	return w
}

// Bindings is the registry, in reference order. Its globals are frozen by TestGlobals: renaming or
// removing one breaks the scripts agents wrote (the tool format is a contract, §8).
func Bindings() []Binding {
	path := Param{Name: "path", Type: "string", Doc: "a home path (relative, ~/…, or absolute under the home) or /shared/<group|unit>/…"}
	content := Param{Name: "content", Type: "string", Doc: "the text to write"}
	return []Binding{
		{Name: "fs.read", Global: "read_file", Params: []Param{path}, Returns: "string: the file's content",
			Doc: "Reads a file of the home or the shared space. Symlinks are refused; at most 8 MiB per run. " +
				"The memory archive (memories/RECORDS.md) and its counters need memory:read besides.",
			Grant: "fs.read:home | fs.read:shared", Example: `local text = read_file("notes/today.md")`, run: fsRead},
		{Name: "fs.write", Global: "write_file", Params: []Param{path, content}, Returns: "nil",
			Doc: "Writes (replaces) a file. Home: protected paths (needletail.yaml, SOUL.md, memories/, sessions/, logs/, state/, skills/, cron/, scripts/, the home's hidden entries, .git/) are refused, missing directories are made. " +
				"Shared: written through the core (fs_write), recorded as yours. At most 4 MiB per run.",
			Grant: "fs.write:home[/glob] | fs.write:shared", Example: `write_file("reports/fares.md", text)`, Write: true, run: fsWrite},
		{Name: "fs.append", Global: "append_file", Params: []Param{path, content}, Returns: "nil",
			Doc: "Appends to a file (made when missing), with fs.write's rules.", Grant: "fs.write:home[/glob] | fs.write:shared",
			Example: `append_file("logs.md", line .. "\n")`, Write: true, run: fsAppend},
		{Name: "fs.list", Global: "list_files", Params: []Param{path}, Returns: "{ {name, type, size} }: the directory's entries (shared: the core's fs_ls answer)",
			Doc: "Lists a directory.", Grant: "fs.read:home | fs.read:shared", Example: `for _, e in list_files("reports") do log("info", e.name) end`, run: fsList},
		{Name: "fs.stat", Global: "stat_file", Params: []Param{path}, Returns: "{type, size, mtime} or nil when missing (shared: the core's fs_info answer)",
			Doc: "Describes a path.", Grant: "fs.read:home | fs.read:shared", Example: `if stat_file("cache.json") then … end`, run: fsStat},
		{Name: "fs.remove", Global: "remove_file", Params: []Param{path}, Returns: "nil",
			Doc: "Removes a home file (not a directory). Shared files cannot be removed from Luau.", Grant: "fs.write:home[/glob]",
			Example: `remove_file("cache.json")`, Write: true, run: fsRemove},
		{Name: "fs.lock", Global: "lock", Params: []Param{path, {Name: "minutes", Type: "number", Optional: true, Doc: "lifetime (the core's default when nil)"}},
			Returns: "table: the core's answer", Doc: "Locks a shared-space path through the core (fs_lock), so other agents see you work on it.",
			Grant: "fs.write:shared", Example: `lock("/shared/group/plan.md", 10)`, Write: true, run: fsLock},
		{Name: "fs.unlock", Global: "unlock", Params: []Param{path}, Returns: "table: the core's answer",
			Doc: "Releases your lock on a shared-space path (fs_unlock).", Grant: "fs.write:shared", Example: `unlock("/shared/group/plan.md")`, Write: true, run: fsUnlock},
		{Name: "http.request", Global: "http_request", Spread: true,
			Params:  []Param{{Name: "request", Type: "table", Doc: "{method = \"GET\", url = \"https://…\", headers = {…}, body = \"…\", timeout_ms = 10000}"}},
			Returns: "{status, headers, body}",
			Doc: "An HTTPS request to a granted host (exact, or *.suffix). Internal addresses are refused, redirects (at most 3) are checked again, " +
				"credentials for the host are added by the runtime (you never see them), the body is cut at 5 MiB, the call at 20 s.",
			Grant: "http:<host>", Example: `local r = http_request{url = "https://api.example.com/fares?from=LIS"}`, Write: true, run: httpRequest},
		{Name: "core.call", Global: "core_call", Params: []Param{{Name: "tool", Type: "string", Doc: "a core tool (msg_send, fs_info, …)"}, {Name: "args", Type: "table", Optional: true}},
			Returns: "table: the tool's answer", Doc: "Calls a swarm core tool as the agent.", Grant: "core:<tool> (core:msg_* by default on a core)",
			Example: `core_call("msg_send", {to = "atlas", text = "fares updated"})`, Write: true, run: coreCall},
		{Name: "memory.read", Global: "read_memory", Returns: "{memory = {…}, user = {…}}: the hot-memory entries",
			Doc: "Reads the agent's memory (read-only; the memory tool changes it).", Grant: "fs.read:home", Example: `local m = read_memory()`, run: memoryRead},
		{Name: "memory.search", Global: "search_memory", Params: []Param{{Name: "query", Type: "string", Doc: `words (synonyms and both languages help), "a phrase", word* a prefix; "" lists newest first`},
			{Name: "opts", Type: "table", Optional: true, Doc: `{ids = {…}, kind = "…", tags = {…}, scope = "all|hot|archive", match = "any|all", since = "YYYY-MM-DD", include_expired = false, limit = 8, audience = "here|all"}`}},
			Returns: "{total, results}: the best records, each {id, kind, tags, hot, imp, created, updated, expires, pii, src, score, text, cut}",
			Doc: "Searches the memory archive (every record, hot or not) by keyword, as memory_search does: BM25 with recency, importance and use; " +
				"ids fetch records whole. It finds what the run's conversation may see (its own records, the agent-wide ones, the hot ones; a schedule's script: the agent-wide and hot ones); " +
				"audience = \"all\" (every conversation's) only in the owner's own runs. memory:read is apart from fs.read:home, which only covers read_memory: the archive holds more client data than the prompt.",
			Grant: "memory:read", Example: `local r = search_memory("cotización quote", {tags = {"cruceros"}})`, run: memorySearch},
		{Name: "memory.save", Global: "save_memory", Params: []Param{{Name: "text", Type: "string", Doc: "the memory, one self-contained note (at most 1000 characters)"},
			{Name: "opts", Type: "table", Optional: true, Doc: `{kind = "fact|preference|procedure|episode|user", tags = {…}, importance = 1-5, expires = "YYYY-MM-DD", force = false, scope = "here|agent"}`}},
			Returns: `{status, id, similar}: status "created", "exists" (id: the same text's) or "similar" (similar: { {id, text, score} }, nothing saved; without memory:read only similar_count)`,
			Doc: "Saves a record to the memory archive (never the always-shown memory: hot is refused), marked as saved by this tool. " +
				"exists: the same text is there (its id); similar: close records, nothing saved (update one, or save with force = true; " +
				"which ones only with memory:read: a writer alone gets similar_count). Secret values are refused. scope \"here\" (the default) is the run's conversation's " +
				"(a schedule's script has none: agent-wide); \"agent\" is everyone's, refused for personal data or a person's profile.",
			Grant: "memory:write", Example: `local r = save_memory("Ana prefers balcony cabins.", {kind = "preference", tags = {"cruceros"}})`, Write: true, run: memorySave},
		{Name: "memory.update", Global: "update_memory", Params: []Param{{Name: "id", Type: "string"},
			{Name: "fields", Type: "table", Doc: `{text, kind, tags, importance, expires} to change (expires = "" clears it)`}},
			Returns: "{id, kind, tags, imp, updated, …, text}: the record as changed (text and tags only with memory:read)",
			Doc:     "Changes an archive record. A hot record (in the system prompt) is the model's: refused, as is any pin.",
			Grant:   "memory:write", Example: `update_memory(id, {expires = "2026-12-31"})`, Write: true, run: memoryUpdate},
		{Name: "memory.delete", Global: "delete_memory", Params: []Param{{Name: "id", Type: "string"}}, Returns: "{deleted = id}",
			Doc: "Deletes an archive record (a hot one is refused).", Grant: "memory:write", Example: `delete_memory(id)`, Write: true, run: memoryDelete},
		{Name: "message.send", Global: "send_message", Params: []Param{{Name: "target", Type: "string", Doc: "where to (send_message's targets)"}, {Name: "text", Type: "string"}},
			Returns: "{platform, chat_id, message_id, home}",
			Doc: "Sends a message (Markdown) through the runtime's messaging policy, as the send_message tool does: the home channel ('slack'), " +
				"a channel or thread the agent is in ('slack:<channel>[:<thread_ts>]'), an allowed user's DM ('slack:<user>'). Never broadcasts: " +
				"@channel, @here, @everyone and user groups are escaped. A target the policy refuses fails with `refused`. Grants: message:slack is the home " +
				"channel, message:slack:<channel>:* a channel and its threads, message:slack:* any target the policy allows.",
			Grant:   "message:<target>",
			Example: `send_message("slack", "done")`, Write: true, run: messageSend},
		{Name: "json.encode", Global: "json_encode", Params: []Param{{Name: "value", Type: "any"}}, Returns: "string",
			Doc: "JSON text of a value: {} for an empty table, an array for keys 1..n, nt.json.null for null.", Example: `local s = json_encode({a = 1})`, Local: true},
		{Name: "json.decode", Global: "json_decode", Params: []Param{{Name: "text", Type: "string"}}, Returns: "any (JSON null is nil)",
			Doc: "Parses JSON text.", Example: `local v = json_decode(r.body)`, Local: true},
		{Name: "log", Global: "log", Params: []Param{{Name: "level", Type: "string", Doc: "debug, info, warn or error"}, {Name: "msg", Type: "string"}},
			Returns: "nil", Doc: "Writes to the run's log (shown to the agent, redacted).", Example: `log("info", "3 fares found")`, run: logLine},
		{Name: "time.now", Global: "now", Returns: "number: Unix time in seconds, with fractions", Doc: "The wall clock (os.time() is the same, in whole seconds).",
			Example: `local t = now()`, run: timeNow},
	}
}

// bindingByName is the registry by name.
func bindingByName() map[string]Binding {
	m := map[string]Binding{}
	for _, b := range Bindings() {
		m[b.Name] = b
	}
	return m
}

// Reference is the registry as markdown, for the system prompt (every function, its arguments,
// result, grant and an example).
func Reference() string {
	var sb strings.Builder
	sb.WriteString("## Luau tool functions\n\n")
	sb.WriteString("A tool is `skills/<skill>/tools/<name>.luau`: a `--[[needletail … ]]` header (format, name, description, args, caps, timeout), " +
		"then `return function(args) … end`. Each function is `nt.<name>` and, where listed, a plain global. A call outside the tool's " +
		"effective capabilities (declared in its header and granted to the agent) fails with `denied`. Errors are Luau errors (`pcall` catches them).\n\n")
	for _, b := range Bindings() {
		args := make([]string, len(b.Params))
		for i, p := range b.Params {
			args[i] = p.Name
			if p.Optional {
				args[i] += "?"
			}
		}
		call := "(" + strings.Join(args, ", ") + ")"
		if b.Spread {
			call = "{…}"
		}
		fmt.Fprintf(&sb, "### nt.%s%s", b.Name, call)
		if b.Global != "" {
			fmt.Fprintf(&sb, " · `%s%s`", b.Global, call)
		}
		sb.WriteString("\n\n" + b.Doc + "\n\n")
		for _, p := range b.Params {
			fmt.Fprintf(&sb, "- `%s` (%s", p.Name, p.Type)
			if p.Optional {
				sb.WriteString(", optional")
			}
			sb.WriteString(")")
			if p.Doc != "" {
				sb.WriteString(": " + p.Doc)
			}
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "- returns %s\n", b.Returns)
		if b.Grant != "" {
			fmt.Fprintf(&sb, "- needs `%s`\n", b.Grant)
		}
		fmt.Fprintf(&sb, "- e.g. `%s`\n\n", b.Example)
	}
	sb.WriteString("Also: `nt.args` (the call's arguments), `nt.json.null`, the standard `string`, `table`, `math`, `utf8`, `bit32`, `buffer` " +
		"libraries, `os.time`, `os.clock`, `collectgarbage(\"count\")` and `print` (captured). There is no file, process, env or secret access " +
		"besides these functions.\n\n")
	sb.WriteString(HandlerReference())
	return sb.String()
}

// CompactReference is one line per function, for the system prompt: its global call (nt.<name>
// when it has none), what it returns and the grant it needs. Reference has the rest.
func CompactReference() string {
	var sb strings.Builder
	for _, b := range Bindings() {
		args := make([]string, len(b.Params))
		for i, p := range b.Params {
			args[i] = p.Name
			if p.Optional {
				args[i] += "?"
			}
		}
		name := b.Global
		if name == "" {
			name = "nt." + b.Name
		}
		call := name + "(" + strings.Join(args, ", ") + ")"
		if b.Spread {
			call = name + "{method, url, headers, body, timeout_ms}"
		}
		ret, _, _ := strings.Cut(b.Returns, " (")
		ret, _, _ = strings.Cut(ret, ": ")
		fmt.Fprintf(&sb, "- `%s` → %s", call, strings.TrimSpace(ret))
		if b.Grant != "" {
			fmt.Fprintf(&sb, "; needs %s", b.Grant)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
