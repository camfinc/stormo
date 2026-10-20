// Package secrets holds the values the runtime itself needs, read once from the container env at
// boot, and the redactor that masks known secret values in tool results, logs and transcripts
// (docs/needletail.md §4.2, §7). Values only ever come from the env; nothing here reads a file.
package secrets

import (
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Passthrough are the secrets left in the env for the terminal: helper scripts call the core as
// the agent with them.
var Passthrough = []string{"SWARM_CORE_URL", "SWARM_CORE_KEY"}

// Names the runtime reads for itself, besides the model and http_secrets key envs in the config.
const (
	APIKey      = "API_SERVER_KEY"
	CoreKey     = "SWARM_CORE_KEY"
	SlackBot    = "SLACK_BOT_TOKEN"
	SlackApp    = "SLACK_APP_TOKEN"
	minRedacted = 8 // shorter values are too likely to be ordinary words
)

// secretName: env names that hold credentials by convention. Their values are redacted (they stay
// in the env when the runtime does not own them: skill scripts read e.g. a CRM token from it).
var secretName = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|API_KEY|_KEY|PRIVATE)`)

// Secrets is the runtime's own credentials.
type Secrets struct {
	values map[string]string
	owned  []string
}

// Owned are the env names the runtime reads for cfg: its API key, the core key, the Slack tokens,
// each model endpoint's key and each http_secrets env (Luau never sees those).
func Owned(cfg *wire.Config) []string {
	names := []string{APIKey, CoreKey, SlackBot, SlackApp}
	add := func(n string) {
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	add(cfg.Model.Primary.KeyEnv)
	for _, f := range cfg.Model.Fallbacks {
		add(f.KeyEnv)
	}
	if cfg.Core != nil {
		add(cfg.Core.KeyEnv)
	}
	for _, h := range cfg.Luau.HTTPSecrets {
		add(h.Env)
	}
	return names
}

// Load reads the owned names from environ ("K=V" pairs, os.Environ outside tests).
func Load(cfg *wire.Config, environ []string) *Secrets {
	env := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	s := &Secrets{values: map[string]string{}, owned: Owned(cfg)}
	for _, n := range s.owned {
		if v := env[n]; v != "" {
			s.values[n] = v
		}
	}
	return s
}

// Get is an owned secret's value ("" when unset).
func (s *Secrets) Get(name string) string { return s.values[name] }

// Scrub unsets the owned secrets from this process' env, except the passthrough allowlist, so
// nothing the runtime starts (the terminal) inherits them.
func (s *Secrets) Scrub() {
	for _, n := range s.owned {
		if !slices.Contains(Passthrough, n) {
			_ = os.Unsetenv(n)
		}
	}
}

// Redactor masks known secret values. It is safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values []string // longest first, so a value containing another is masked whole
	r      *strings.Replacer
}

// Mask replaces a secret value.
const Mask = "[REDACTED]"

// NewRedactor masks the runtime's own secrets and every env value whose name looks like a
// credential (environ: os.Environ, read before Scrub).
func NewRedactor(s *Secrets, environ []string) *Redactor {
	r := &Redactor{}
	if s != nil {
		for _, v := range s.values {
			r.add(v)
		}
	}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && secretName.MatchString(k) {
			r.add(v)
		}
	}
	r.build()
	return r
}

// Add masks one more value (from now on).
func (r *Redactor) Add(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.add(v)
	r.build()
}

func (r *Redactor) add(v string) {
	v = strings.TrimSpace(v)
	if len(v) >= minRedacted && !slices.Contains(r.values, v) {
		r.values = append(r.values, v)
	}
}

func (r *Redactor) build() {
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
	pairs := make([]string, 0, 2*len(r.values))
	for _, v := range r.values {
		pairs = append(pairs, v, Mask)
	}
	r.r = strings.NewReplacer(pairs...)
}

// Redact is s with every known secret value masked.
func (r *Redactor) Redact(s string) string {
	if r == nil {
		return s
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.values) == 0 {
		return s
	}
	return r.r.Replace(s)
}

// Writer redacts each Write to w (a log destination).
func (r *Redactor) Writer(w io.Writer) io.Writer { return redactingWriter{r, w} }

type redactingWriter struct {
	r *Redactor
	w io.Writer
}

func (rw redactingWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(rw.w, rw.r.Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}
