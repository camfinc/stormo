package luau

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/camfinc/stormo/internal/needletail/followup"
	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/mcp"
	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Per-run quotas and bounds of the host side.
const (
	ReadQuota  = 8 << 20 // bytes nt.fs reads in one run
	WriteQuota = 4 << 20 // bytes nt.fs writes in one run
	maxList    = 1000    // entries of one fs.list
	maxLogs    = 64 << 10
	// SharedPrefix is how scripts name the shared space (the agents' mount point).
	SharedPrefix = "/shared"
)

// CoreCaller calls the swarm core's tools as the agent (mcp.Client).
type CoreCaller interface {
	Call(ctx context.Context, name string, args json.RawMessage) (*mcp.Result, error)
}

// Runtime is what Luau runs against in one agent home. Fields are set once at boot.
type Runtime struct {
	Runner Runner
	Home   string
	// SharedDir is where the shared space is mounted here ($SWARM_SHARED_DIR); scripts name it
	// /shared. Layers are the shared directories this agent has: group and its unit.
	SharedDir string
	Layers    []string
	// Grants are needletail.yaml's luau.grants; HTTPSecrets its http_secrets, whose values
	// Secret gives (by env name).
	Grants      []string
	HTTPSecrets []wire.HTTPSecret
	Secret      func(env string) string
	// Core is the swarm core (nil when the agent is not on one); CoreURLs its addresses (the MCP
	// and ingest URLs), which nt.http never dials.
	Core     CoreCaller
	CoreURLs []string
	Memory   *memory.Store
	// FollowUps are the agent's one-time follow-ups (follow_up, nt.followup; nil: unavailable).
	FollowUps *followup.Queue
	// Messenger sends send_message's messages under the runtime's messaging policy (the Slack
	// gateway; nil: none runs, and send_message is unavailable).
	Messenger messaging.Messenger
	// Redact masks secret values in every answer a script gets, its logs and results (the
	// http_secrets values are masked besides, in any JSON spelling: redact.go).
	Redact func(string) string
	Log    func(string)
	// LuauTimeout bounds a tool's run, as the model calls it or luau_test does
	// (limits.luau_timeout); ScriptTimeout a schedule's .luau script (limits.script_timeout). A
	// header's timeout may only lower either; unset, each is DefaultTimeout.
	LuauTimeout   time.Duration
	ScriptTimeout time.Duration
	Now           func() time.Time
	// Untrusted is the persisted conversation provenance, resolved by the host.
	Untrusted func(context.Context, string) string
	// Observer, when set, gives the observer of handler runs (the core's hooks, which connect
	// after boot): each run is a tool_start and tool_done named luau:<skill>__<name>#<event>.
	Observer func() HookObserver

	// tests only: extra roots for nt.http's TLS, and a resolver standing in for DNS
	rootCAs *tls.Config
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	auditMu sync.Mutex
	maskMu  sync.Mutex
	mask    secretMasker
	kv      kvStore // the agent's key-value store (kv.go)
}

func (rt *Runtime) redact(s string) string {
	if rt.Redact != nil {
		s = rt.Redact(s)
	}
	return rt.maskSecrets(s)
}

func (rt *Runtime) logf(format string, args ...any) {
	if rt.Log != nil {
		rt.Log(rt.redact(fmt.Sprintf(format, args...)))
	}
}

func (rt *Runtime) now() time.Time {
	if rt.Now != nil {
		return rt.Now()
	}
	return time.Now()
}

func (rt *Runtime) timeout(h *Header, script bool) time.Duration {
	t := rt.LuauTimeout
	if script {
		t = rt.ScriptTimeout
	}
	if t <= 0 {
		t = DefaultTimeout
	}
	if h != nil && h.Timeout > 0 && h.Timeout < t {
		t = h.Timeout
	}
	return t
}

// Call is one run's host: the script, its effective capabilities and what it used. It is the
// engine's Host.
type Call struct {
	rt      *Runtime
	ctx     context.Context
	Tool    string
	Session string
	Caps    Caps
	// DryRun refuses every binding that writes or reaches out (luau_test).
	DryRun bool

	mu      sync.Mutex
	read    int64
	wrote   int64
	touched []string // /shared paths written (the hooks attribute them)
	logs    []string
	logSize int
	logCut  bool
	denied  []string
}

// NewCall is a run of tool (its name, for the audit) in session with caps.
func (rt *Runtime) NewCall(tool, session string, caps Caps, dryRun bool) *Call {
	return &Call{rt: rt, Tool: tool, Session: session, Caps: caps, DryRun: dryRun}
}

// runContext keeps one mutable quarantine state across every binding of a run.
// WithQuarantine preserves an existing loop persistence callback.
func (c *Call) runContext(ctx context.Context) context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx == nil {
		c.ctx = messaging.WithQuarantine(ctx, nil)
	}
	return messaging.ShareQuarantine(ctx, c.ctx)
}
func (c *Call) UntrustedWhy() string {
	c.mu.Lock()
	ctx := c.ctx
	c.mu.Unlock()
	if ctx == nil {
		return ""
	}
	return messaging.UntrustedWhy(ctx)
}
func (c *Call) quarantined(ctx context.Context) string {
	if why := messaging.UntrustedWhy(ctx); why != "" {
		return why
	}
	if c.rt.Untrusted != nil {
		return c.rt.Untrusted(ctx, c.Session)
	}
	return ""
}

// Write-capable bindings require an explicit quarantine path. Unknown future
// writers fail closed; these exceptions have their own gates or only reach the
// external network (which quarantine does not constrain).
func quarantineWrite(name string, b Binding) bool {
	if !b.Write {
		return false
	}
	switch name {
	case "core.call", "http.request", "message.send", "kv.set", "kv.delete", "followup.create", "followup.cancel":
		return false
	}
	return true
}

// Touched are the shared-space paths the run wrote (as /shared/<layer>/…).
func (c *Call) Touched() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.touched...)
}

// Logs are the run's nt.log lines (redacted).
func (c *Call) Logs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.logs...)
}

// Denied are the calls refused for a capability, as "<binding> <cap>".
func (c *Call) Denied() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.denied...)
}

var registry = bindingByName()

// hostError is a refusal with a code the script sees ("denied: …").
type hostError struct{ code, msg string }

func (e *hostError) Error() string { return e.code + ": " + e.msg }

func errf(code, format string, args ...any) error {
	return &hostError{code: code, msg: fmt.Sprintf(format, args...)}
}

func (c *Call) deny(binding, capability string) error {
	c.mu.Lock()
	c.denied = append(c.denied, binding+" "+capability)
	c.mu.Unlock()
	return errf("denied", "%s needs %s, which this tool does not have (its caps, as granted)", binding, capability)
}

// Call answers one host call (Host).
func (c *Call) Call(ctx context.Context, name string, in []byte) []byte {
	ctx = c.runContext(ctx)
	b, ok := registry[name]
	var v any
	var err error
	switch {
	case !ok || b.Local || b.run == nil:
		err = errf("unknown", "no binding %q", name)
	case quarantineWrite(name, b) && c.quarantined(ctx) != "":
		err = errf("refused", "%v", messaging.Deferred(name, c.quarantined(ctx)))
	case c.DryRun && b.Write:
		err = errf("dry_run", "%s is not run in a dry run (luau_test): writes, http and core calls are off", name)
	default:
		raw := json.RawMessage(in)
		if !json.Valid(raw) || len(raw) == 0 || raw[0] != '{' {
			err = errf("bad_args", "%s: arguments must be an object", name)
		} else {
			v, err = b.run(ctx, c, raw)
		}
	}
	var out []byte
	status := "ok"
	if err != nil {
		code, msg := "error", err.Error()
		var he *hostError
		if errors.As(err, &he) {
			code, msg = he.code, he.msg
		}
		status = code
		out = failure(code, c.rt.redact(msg))
	} else {
		out, err = json.Marshal(map[string]any{"ok": true, "v": c.rt.scrub(v)})
		if err != nil {
			status, out = "error", failure("error", "the answer could not be encoded")
		}
	}
	// Nothing a script gets back holds a secret value: an echo of an injected header included,
	// masked in its strings before the encoding (scrub) and in the encoded answer after.
	out = []byte(c.rt.redact(string(out)))
	c.audit(name, b.Grant, in, len(out), status)
	return out
}

// audit appends the call to logs/audit/<session>.jsonl (Raw: its arguments are hashed, not kept).
func (c *Call) audit(binding, capability string, in []byte, out int, status string) {
	if c.rt.Home == "" {
		return
	}
	sum := sha256.Sum256(in)
	line, _ := json.Marshal(map[string]any{"ts": c.rt.now().UTC().Format(time.RFC3339Nano), "tool": c.Tool, "call": binding,
		"cap": capability, "args_sha256": hex.EncodeToString(sum[:]), "bytes_in": len(in), "bytes_out": out, "status": status})
	session := c.Session
	if session == "" {
		session = "none"
	}
	dir := filepath.Join(c.rt.Home, "logs", "audit")
	c.rt.auditMu.Lock()
	defer c.rt.auditMu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, safeName(session)+".jsonl"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(line, '\n'))
	_ = f.Close()
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func safeName(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	if len(s) > 120 {
		s = s[:120]
	}
	if s == "" || s == "." || s == ".." {
		s = "_"
	}
	return s
}

// ---- places --------------------------------------------------------------------------------

// place is a path a script named: in the home (rel to it) or in a shared layer.
type place struct {
	shared bool
	root   string // the home, or the layer's directory
	rel    string // slash-separated, relative to root ("" for root itself)
	agent  string // the shared path as agents and the core name it: /shared/<layer>/<rel>
}

// protected are the home's top-level entries a script never writes: the runtime's own files, the
// agent's memory (the memory tool), its skills (skill_manage checks every change), its schedules
// (cronjob_manage) and the job scripts they run (scripts/: the baseline's, and the ones the agent
// writes with its own file tools or terminal), and the temp dir.
var protected = []string{"needletail.yaml", "SOUL.md", "sessions", "logs", "state", "memories", "skills", "cron", "scripts", ".needletail-tmp", ".swarm"}

// isProtected: rel (slash-separated, relative to the home) is a protected entry or below one, a
// hidden entry of the home's root or below one (shell startup files, .config, .local's Python
// user site, .gitconfig, .ssh: files a program runs without being told to), a Python module or
// package at the home's root (the terminal's cwd, first on sys.path for python3 -c: json.py
// would shadow the standard json), or inside a git repository's .git (hooks, config). Names
// compare case-folded: on a case-insensitive home Skills/ is skills/.
func isProtected(rel string) bool {
	segs := strings.Split(rel, "/")
	if strings.HasPrefix(segs[0], ".") {
		return true
	}
	last := strings.ToLower(segs[len(segs)-1])
	if len(segs) == 1 && (strings.HasSuffix(last, ".py") || strings.HasSuffix(last, ".pyc")) ||
		len(segs) == 2 && (last == "__init__.py" || last == "__init__.pyc") {
		return true
	}
	for _, p := range protected {
		if strings.EqualFold(segs[0], p) {
			return true
		}
	}
	for _, s := range segs {
		if strings.EqualFold(s, ".git") {
			return true
		}
	}
	return false
}

// place reads a script's path: lexically cleaned, never outside its root, and with no symlink in
// any component that exists (each is checked with Lstat).
func (c *Call) place(p string) (place, error) {
	if strings.TrimSpace(p) == "" {
		return place{}, errf("bad_args", "empty path")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return place{}, errf("bad_args", "the path has a control character")
		}
	}
	var pl place
	clean := path.Clean(filepath.ToSlash(p))
	shared := filepath.ToSlash(filepath.Clean(c.rt.SharedDir))
	switch {
	case clean == SharedPrefix || strings.HasPrefix(clean, SharedPrefix+"/") || c.rt.SharedDir != "" && (clean == shared || strings.HasPrefix(clean, shared+"/")):
		rest := strings.TrimPrefix(strings.TrimPrefix(clean, SharedPrefix), "/")
		if !strings.HasPrefix(clean, SharedPrefix) {
			rest = strings.TrimPrefix(strings.TrimPrefix(clean, shared), "/")
		}
		layer, rel, _ := strings.Cut(rest, "/")
		ok := false
		for _, l := range c.rt.Layers {
			if l == layer && layer != "" {
				ok = true
			}
		}
		if !ok || c.rt.SharedDir == "" {
			return place{}, errf("outside", "%s: only /shared/<%s> is reachable", p, strings.Join(c.rt.Layers, "|"))
		}
		pl = place{shared: true, root: filepath.Join(c.rt.SharedDir, layer), rel: rel, agent: path.Join(SharedPrefix, layer, rel)}
	default:
		rel := clean
		switch {
		case clean == "~":
			rel = "."
		case strings.HasPrefix(clean, "~/"):
			rel = strings.TrimPrefix(clean, "~/")
		case path.IsAbs(clean):
			home := filepath.ToSlash(filepath.Clean(c.rt.Home))
			if clean != home && !strings.HasPrefix(clean, home+"/") {
				return place{}, errf("outside", "%s is outside the home and the shared space", p)
			}
			rel = strings.TrimPrefix(strings.TrimPrefix(clean, home), "/")
		}
		rel = path.Clean("/" + rel)[1:] // never above the home
		if strings.HasPrefix(path.Clean(filepath.ToSlash(p)), "..") {
			return place{}, errf("outside", "%s leaves the home", p)
		}
		pl = place{root: c.rt.Home, rel: rel}
	}
	if pl.rel == "." {
		pl.rel = ""
	}
	// every existing component below the root: no symlinks
	cur := pl.root
	if pl.rel != "" {
		for _, seg := range strings.Split(pl.rel, "/") {
			cur = filepath.Join(cur, seg)
			info, err := os.Lstat(cur)
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil {
				return place{}, errf("io", "%s: %v", p, err)
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				return place{}, errf("symlink", "%s: a symlink is in the way (Luau never follows symlinks)", p)
			}
		}
	}
	if placed != nil {
		placed(pl.rel)
	}
	return pl, nil
}

// placed (tests only) runs once a path is read, before the file system is touched: where a
// concurrent change of the tree lands. readStatted (tests only) runs once fs.read measured its file.
var placed, readStatted func(rel string)

func scope(p place) string {
	if p.shared {
		return "shared"
	}
	return "home"
}

type pathArgs struct {
	Path    string   `json:"path"`
	Content *string  `json:"content"`
	Minutes *float64 `json:"minutes"`
}

func pathArg(in json.RawMessage) (pathArgs, error) {
	var a pathArgs
	if err := json.Unmarshal(in, &a); err != nil {
		return a, errf("bad_args", "path must be a string (and content a string): %v", err)
	}
	return a, nil
}

func (c *Call) readPlace(name string, in json.RawMessage) (place, error) {
	a, err := pathArg(in)
	if err != nil {
		return place{}, err
	}
	p, err := c.place(a.Path)
	if err != nil {
		return place{}, err
	}
	if !p.shared {
		first, _, _ := strings.Cut(p.rel, "/")
		for _, private := range []string{"sessions", "state", "logs"} {
			if strings.EqualFold(first, private) {
				return place{}, errf("refused", "%s is private runtime data: use the model's session_search for history, search_memory for memory, or kv_get for KV so provenance is preserved", display(p))
			}
		}
	}
	if !c.Caps.FSRead(scope(p)) {
		return place{}, c.deny(name, "fs.read:"+scope(p))
	}
	return p, nil
}

func (c *Call) writePlace(name string, a pathArgs) (place, error) {
	p, err := c.place(a.Path)
	if err != nil {
		return place{}, err
	}
	if p.shared {
		if !c.Caps.FSWrite("shared", "") {
			return place{}, c.deny(name, "fs.write:shared")
		}
		return p, nil
	}
	if p.rel == "" || isProtected(p.rel) {
		return place{}, errf("protected", "%s is protected: Luau never writes the runtime's files, memories/, skills/, cron/, scripts/, logs/, "+
			"the home's hidden entries (.profile, .config, …) or a .git directory", a.Path)
	}
	if !c.Caps.FSWrite("home", p.rel) {
		return place{}, c.deny(name, "fs.write:home/"+p.rel)
	}
	return p, nil
}

// readLeft is what the run may still read.
func (c *Call) readLeft() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ReadQuota - c.read
}

func (c *Call) quota(read, wrote int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.read+read > ReadQuota {
		return errf("quota", "the run's read quota (%d bytes) is used up", ReadQuota)
	}
	if c.wrote+wrote > WriteQuota {
		return errf("quota", "the run's write quota (%d bytes) is used up", WriteQuota)
	}
	c.read += read
	c.wrote += wrote
	return nil
}

func fsRead(_ context.Context, c *Call, in json.RawMessage) (any, error) {
	p, err := c.readPlace("fs.read", in)
	if err != nil {
		return nil, err
	}
	f, err := p.openFile(os.O_RDONLY, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, ioErr(p, err)
	}
	if !p.shared && c.memoryFile(p.rel, info) {
		if !c.Caps.Memory("read") {
			return nil, c.deny("fs.read", "memory:read")
		}
		if c.rt.Memory == nil || !c.audience(c.rt.Memory).Operator {
			return nil, errf("refused", "%s is the memory's store, every conversation's records: read_file shows it only in the owner's own run (a oneshot); "+
				"search_memory finds the records this run may see", display(p))
		}
	}
	if readStatted != nil {
		readStatted(p.rel)
	}
	// refused early by its size, then read only up to what is left of the quota (the file may
	// grow), and charged what was read
	left := c.readLeft()
	if info.Size() > left {
		return nil, errf("quota", "the run's read quota (%d bytes) is used up", ReadQuota)
	}
	b, err := io.ReadAll(io.LimitReader(f, left+1))
	if err != nil {
		return nil, ioErr(p, err)
	}
	if err := c.quota(int64(len(b)), 0); err != nil {
		return nil, err
	}
	return string(b), nil
}

// memoryFile: the file a script opened at rel (in the home) is one of the memory's store files
// (the archive, its counters, the lock file) or a write's temporary copy of one, which
// fs.read:home does not cover: the archive holds every audience's records (§4.6, Scopes), more
// client data than the hot files, which read_memory and fs.read:home give, so only the owner's
// own run reads it raw (fsRead); everyone else searches it in their audience. Matched by the
// file itself (the opened descriptor's), so another spelling of the path (case, on a
// case-insensitive disk; the path is cleaned by place) or a hard link is the same file; place
// and openFile never follow a symlink.
func (c *Call) memoryFile(rel string, info os.FileInfo) bool {
	for _, f := range []string{memory.RecordsFile, memory.UsageFile, memory.LockFile} {
		if strings.EqualFold(rel, f) {
			return true
		}
		if st, err := os.Stat(filepath.Join(c.rt.Home, filepath.FromSlash(f))); err == nil && os.SameFile(st, info) {
			return true
		}
		if dir, base := path.Split(rel); strings.EqualFold(path.Clean(dir), fsx.TmpDir) &&
			strings.HasPrefix(strings.ToLower(base), "."+strings.ToLower(path.Base(f))+".tmp") {
			return true
		}
	}
	return false
}

func display(p place) string {
	if p.shared {
		return p.agent
	}
	if p.rel == "" {
		return "~"
	}
	return p.rel
}

func ioErr(p place, err error) error {
	var he *hostError
	switch {
	case errors.As(err, &he):
		return he
	case errors.Is(err, fs.ErrNotExist):
		return errf("not_found", "%s does not exist", display(p))
	case errors.Is(err, syscall.ELOOP):
		return errf("symlink", "%s: a symlink is in the way (Luau never follows symlinks)", display(p))
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return errf("io", "%s: %v", display(p), err)
}

func fsWrite(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	return c.write(ctx, "fs.write", in, false)
}

func fsAppend(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	return c.write(ctx, "fs.append", in, true)
}

func (c *Call) write(ctx context.Context, name string, in json.RawMessage, appendTo bool) (any, error) {
	a, err := pathArg(in)
	if err != nil {
		return nil, err
	}
	if a.Content == nil {
		return nil, errf("bad_args", "%s needs content (a string)", name)
	}
	p, err := c.writePlace(name, a)
	if err != nil {
		return nil, err
	}
	if err := c.quota(0, int64(len(*a.Content))); err != nil {
		return nil, err
	}
	if p.shared {
		mode := "overwrite"
		if appendTo {
			mode = "append"
		}
		if _, err := c.core(ctx, "fs_write", map[string]any{"path": p.agent, "content": *a.Content, "mode": mode}); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.touched = append(c.touched, p.agent)
		c.mu.Unlock()
		return nil, nil
	}
	// directories made on the way, the file opened without truncating, then truncated once it is
	// known to be a regular file
	flags := os.O_WRONLY | os.O_CREATE
	if appendTo {
		flags |= os.O_APPEND
	}
	f, err := p.openFile(flags, true)
	if err != nil {
		return nil, err
	}
	if !appendTo {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, ioErr(p, err)
		}
	}
	_, werr := f.WriteString(*a.Content)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return nil, ioErr(p, errors.Join(werr, cerr))
	}
	return nil, nil
}

func fsList(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	p, err := c.readPlace("fs.list", in)
	if err != nil {
		return nil, err
	}
	if routed, err := c.viaCore(p, "fs.list"); err != nil || routed {
		if err != nil {
			return nil, err
		}
		return c.core(ctx, "fs_ls", map[string]any{"path": p.agent})
	}
	return p.list(ctx, maxList)
}

// viaCore: a shared read the core answers (fs_ls, fs_info). A dry run never calls the core: it
// reads the mount when this container has it, else the call is refused.
func (c *Call) viaCore(p place, name string) (bool, error) {
	if !p.shared || c.rt.Core == nil {
		return false, nil
	}
	if !c.DryRun {
		return true, nil
	}
	if info, err := os.Stat(p.root); err != nil || !info.IsDir() {
		return false, errf("dry_run", "%s of %s goes through the core, which a dry run (luau_test) does not call, and the shared space is not mounted here", name, p.agent)
	}
	return false, nil
}

func fsStat(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	p, err := c.readPlace("fs.stat", in)
	if err != nil {
		return nil, err
	}
	if routed, err := c.viaCore(p, "fs.stat"); err != nil || routed {
		if err != nil {
			return nil, err
		}
		return c.core(ctx, "fs_info", map[string]any{"path": p.agent})
	}
	st, err := p.lstat()
	var he *hostError
	if errors.As(err, &he) && he.code == "not_found" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	mtime, _ := st.Mtim.Unix()
	return map[string]any{"type": statKind(st), "size": st.Size, "mtime": mtime}, nil
}

func fsRemove(_ context.Context, c *Call, in json.RawMessage) (any, error) {
	a, err := pathArg(in)
	if err != nil {
		return nil, err
	}
	p, err := c.place(a.Path)
	if err != nil {
		return nil, err
	}
	if p.shared {
		return nil, errf("unsupported", "shared files cannot be removed from Luau")
	}
	if p, err = c.writePlace("fs.remove", a); err != nil {
		return nil, err
	}
	return nil, p.remove()
}

func fsLock(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	return c.lock(ctx, "fs.lock", "fs_lock", in)
}

func fsUnlock(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	return c.lock(ctx, "fs.unlock", "fs_unlock", in)
}

func (c *Call) lock(ctx context.Context, name, tool string, in json.RawMessage) (any, error) {
	a, err := pathArg(in)
	if err != nil {
		return nil, err
	}
	p, err := c.place(a.Path)
	if err != nil {
		return nil, err
	}
	if !p.shared {
		return nil, errf("unsupported", "%s: locks are for the shared space", name)
	}
	if !c.Caps.FSWrite("shared", "") {
		return nil, c.deny(name, "fs.write:shared")
	}
	args := map[string]any{"path": p.agent}
	if a.Minutes != nil && tool == "fs_lock" {
		args["minutes"] = *a.Minutes
	}
	return c.core(ctx, tool, args)
}

// ---- core, memory, log, time, messages -----------------------------------------------------

var coreTool = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func coreCall(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	var a struct {
		Tool string          `json:"tool"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(in, &a); err != nil || !coreTool.MatchString(a.Tool) {
		return nil, errf("bad_args", "core.call needs a tool name and an args table")
	}
	if !c.Caps.Core(a.Tool) {
		return nil, c.deny("core.call", "core:"+a.Tool)
	}
	args := map[string]any{}
	if len(a.Args) > 0 && string(a.Args) != "null" {
		if err := json.Unmarshal(a.Args, &args); err != nil {
			return nil, errf("bad_args", "core.call: args must be a table with string keys")
		}
	}
	v, err := c.core(ctx, a.Tool, args)
	// A shared write through the core is the script's: the hooks attribute it (once it is done).
	if p, ok := args["path"].(string); ok && err == nil && a.Tool == "fs_write" && strings.HasPrefix(path.Clean(p), SharedPrefix+"/") {
		c.mu.Lock()
		c.touched = append(c.touched, path.Clean(p))
		c.mu.Unlock()
	}
	return v, err
}

// core calls one of the core's tools: an object answer as it is, any other text as {result}.
func (c *Call) core(ctx context.Context, tool string, args map[string]any) (any, error) {
	if why := c.quarantined(ctx); why != "" && !messaging.ReadOnlyCoreTool(tool) {
		return nil, errf("refused", "%v", messaging.Deferred("core_call "+tool, why))
	}
	if c.rt.Core == nil {
		return nil, errf("unavailable", "%s: the agent is not on a swarm core", tool)
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil, errf("bad_args", "%s: %v", tool, err)
	}
	res, err := c.rt.Core.Call(ctx, tool, b)
	if err != nil {
		if mcp.Away(err) {
			return nil, errf("core_away", "%s: the swarm core is unreachable right now", tool)
		}
		return nil, errf("core", "%s: %v", tool, err)
	}
	text := strings.TrimSpace(res.Text)
	if res.IsError {
		return nil, errf("core", "%s: %s", tool, text)
	}
	if strings.HasPrefix(text, "{") && json.Valid([]byte(text)) {
		return json.RawMessage(text), nil
	}
	return map[string]string{"result": text}, nil
}

func memoryRead(_ context.Context, c *Call, _ json.RawMessage) (any, error) {
	if !c.Caps.FSRead("home") {
		return nil, c.deny("memory.read", "fs.read:home")
	}
	if c.rt.Memory == nil {
		return map[string]any{"memory": []string{}, "user": []string{}}, nil
	}
	out := map[string]any{}
	for _, t := range []string{memory.TargetMemory, memory.TargetUser} {
		e, err := c.rt.Memory.Entries(t)
		if err != nil {
			return nil, errf("io", "memory: %v", err)
		}
		if e == nil {
			e = []string{}
		}
		out[t] = e
	}
	return out, nil
}

var levels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

func logLine(_ context.Context, c *Call, in json.RawMessage) (any, error) {
	var a struct {
		Level any `json:"level"`
		Msg   any `json:"msg"`
	}
	_ = json.Unmarshal(in, &a)
	level, _ := a.Level.(string)
	if !levels[level] {
		level = "info"
	}
	msg := fmt.Sprint(a.Msg)
	if s, ok := a.Msg.(string); ok {
		msg = s
	}
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	line := c.rt.redact(level + ": " + msg)
	c.mu.Lock()
	keep := c.logSize+len(line) <= maxLogs
	first := !keep && !c.logCut
	if keep {
		c.logs = append(c.logs, line)
		c.logSize += len(line)
	} else if first {
		// past the quota: one notice, in the run's log and the runtime's, then nothing more
		c.logCut = true
		c.logs = append(c.logs, "warn: the log is over its 64 KiB; later lines are dropped (truncated)")
	}
	c.mu.Unlock()
	switch {
	case keep:
		c.rt.logf("luau %s: %s", c.Tool, line)
	case first:
		c.rt.logf("luau %s: log truncated at %d bytes; later lines are dropped", c.Tool, maxLogs)
	}
	return nil, nil
}

func timeNow(_ context.Context, c *Call, _ json.RawMessage) (any, error) {
	return float64(c.rt.now().UnixMicro()) / 1e6, nil
}

// messageSend sends a message through the runtime's one messaging policy (the gateway's, as the
// send_message tool does), within the tool's message:<target> caps. It never broadcasts: every
// @channel, @here, @everyone or user group in the text is escaped.
func messageSend(ctx context.Context, c *Call, in json.RawMessage) (any, error) {
	var a struct {
		Target any `json:"target"`
		Text   any `json:"text"`
	}
	_ = json.Unmarshal(in, &a)
	target, _ := a.Target.(string)
	text, _ := a.Text.(string)
	if strings.TrimSpace(text) == "" {
		return nil, errf("bad_args", "send_message needs a target and a text")
	}
	t, err := messaging.Parse(target)
	if err != nil {
		return nil, errf("bad_args", "send_message: %v", err)
	}
	if !c.Caps.Message(t.String()) {
		return nil, c.deny("message.send", "message:"+t.String())
	}
	if messaging.HandlerEvent(ctx) == BeforeSend {
		// a before_send handler decides on a message; sending one of its own would start another
		return nil, errf("refused", "send_message is not available in a before_send handler: allow, rewrite or block the message")
	}
	if c.rt.Messenger == nil {
		return nil, errf("unavailable", "send_message: no messaging gateway runs here (Slack is off)")
	}
	sent, err := c.rt.Messenger.Send(messaging.WithSession(ctx, c.Session), messaging.Request{Target: t.String(), Text: text})
	switch {
	case errors.Is(err, messaging.ErrRefused):
		return nil, errf("refused", "send_message: %v", err)
	case err != nil:
		return nil, errf("message", "send_message: %v", err)
	}
	return sent, nil
}
