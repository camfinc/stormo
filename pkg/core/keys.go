// Package core is the swarm's control plane on the operator's machine: the model gateway for
// local agents, the fleet registry and the office UI.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"regexp"
	"sort"
	"sync"

	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
)

// Who is calling: every request into the core carries `Authorization: Bearer <SWARM_CORE_KEY>`,
// a per-agent, local-only key kept in secrets.local.yaml under `local.agents.<id>` (minted by
// `stormo secrets init`). The caller's identity comes from the key, never from a field it sends.
// The file is re-read when it changes, so a new key works without restarting the core.

const minKeyLength = 16

var bearer = regexp.MustCompile(`(?i)^Bearer\s+(\S+)$`)

func digest(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:])
}

// AgentKeys maps core keys (by digest) to agent ids.
type AgentKeys struct {
	path     string
	mu       sync.Mutex
	mtime    int64
	byDigest map[string]string
}

// NewAgentKeys reads keys from the secrets file at path.
func NewAgentKeys(path string) *AgentKeys {
	return &AgentKeys{path: path, mtime: -1, byDigest: map[string]string{}}
}

// reload re-reads the file when its mtime changed. Caller holds k.mu.
func (k *AgentKeys) reload() {
	var m int64
	if info, err := os.Stat(k.path); err == nil {
		m = info.ModTime().UnixNano()
	}
	if m == k.mtime {
		return
	}
	k.mtime = m
	next := map[string]string{}
	if f, err := secrets.Load(k.path); err == nil && f.Local != nil {
		for _, agent := range f.Local.Agents.Keys() {
			if key, ok := f.Local.Agents.Get(agent).Get(manifest.Core.KeyEnv); ok && len(key) >= minKeyLength {
				next[digest(key)] = agent
			}
		}
	}
	k.byDigest = next
}

// AgentFor is the agent a bearer header belongs to. Lookup is by digest, so no key is compared
// char by char.
func (k *AgentKeys) AgentFor(authorization string) (string, bool) {
	m := bearer.FindStringSubmatch(authorization)
	if m == nil {
		return "", false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reload()
	a, ok := k.byDigest[digest(m[1])]
	return a, ok
}

// Agents are the agents with a core key, sorted.
func (k *AgentKeys) Agents() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.reload()
	seen := map[string]bool{}
	out := []string{}
	for _, a := range k.byDigest {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}
