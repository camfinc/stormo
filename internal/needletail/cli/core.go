package cli

import (
	"context"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/hooks"
	"github.com/camfinc/stormo/internal/needletail/mcp"
)

// coreLink is the runtime's ties to the swarm core (docs/needletail.md §4.10, §4.11): the core's
// tools in the registry, hooks for every turn, and an fs_note after each file-tool write under
// /shared. All of it goes through the sidecar's relay (cfg.Core), which holds what it can while
// the core is away; none of it ever blocks a turn.
type coreLink struct {
	hooks  *hooks.Sender
	notes  *mcp.Background
	cancel context.CancelFunc
}

// Retry bounds for listing the core's tools at boot.
const (
	toolsFirstRetry = 2 * time.Second
	toolsMaxRetry   = time.Minute
	notesQueue      = 64
)

// connectCore wires rt to its core when the config names one. It runs in the background until
// close; nil when the agent is not on a core.
func connectCore(rt *runtime) *coreLink {
	c := rt.cfg.Core
	if c == nil {
		rt.logger.Printf("not on a swarm core: no core tools, no hooks")
		return nil
	}
	key := rt.sec.Get(c.KeyEnv)
	if key == "" {
		rt.logger.Printf("%s is not set: no core tools, no hooks", c.KeyEnv)
		return nil
	}
	logf := func(s string) { rt.logger.Println(s) }
	// Not the serve context: hooks and notes outlive SIGTERM while running turns drain.
	ctx, cancel := context.WithCancel(context.Background())
	client := mcp.New(c.MCPURL, key, logf)
	go client.Register(ctx, rt.agent.Tools, toolsFirstRetry, toolsMaxRetry)

	notes := client.Background(ctx, notesQueue)
	shared := sharedPath(rt.cfg)
	rt.agent.Env.SharedWrite = func(_ context.Context, real string) {
		if p, ok := shared(real); ok {
			notes.Go("fs_note", map[string]string{"path": p})
		}
	}

	h := hooks.New(hooks.Options{URL: c.IngestURL, Key: key, Redact: rt.red.Redact, Log: logf,
		Session: func(id string) (string, string) {
			root, ok := rt.store.Root(id)
			if !ok {
				return id, ""
			}
			s, _ := rt.store.Get(root)
			return root, s.Source
		}})
	h.Start(ctx)
	rt.agent.Observer = h
	rt.logger.Printf("swarm core: tools from %s, hooks to %s/%s", c.MCPURL, strings.TrimSuffix(c.IngestURL, "/"), hooks.Kind)
	return &coreLink{hooks: h, notes: notes, cancel: cancel}
}

// close gives queued notes and hooks (the last turns' session_end) up to wait to go, then stops.
func (l *coreLink) close(wait time.Duration) {
	if l == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	l.notes.Flush(ctx)
	l.hooks.Flush(ctx)
	cancel()
	l.cancel()
}

// sharedPath turns a file tool's resolved path into the shared-space path the core knows
// (/shared/<layer>/…), whatever the shared root is mounted as here.
func sharedPath(cfg *config.Config) func(real string) (string, bool) {
	root := filepath.Clean(cfg.SharedDir)
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	return func(real string) (string, bool) {
		rel, err := filepath.Rel(root, real)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", false
		}
		return path.Join(config.DefaultSharedDir, filepath.ToSlash(rel)), true
	}
}
