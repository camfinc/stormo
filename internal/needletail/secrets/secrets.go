// Package secrets holds the values the runtime itself needs, read once from the container env at
// boot, and the redactor that masks known secret values in tool results, logs and transcripts
// (docs/needletail.md §4.2, §7). Values only ever come from the env; nothing here reads a file.
package secrets

import (
	"encoding/json"
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
	LearningKey = "NEEDLETAIL_LEARNING_KEY"
	CoreKey     = "SWARM_CORE_KEY"
	SlackBot    = "SLACK_BOT_TOKEN"
	SlackApp    = "SLACK_APP_TOKEN"
	// STTKey is the speech-to-text provider's key (ElevenLabs'), the runtime's when stt is on.
	STTKey      = "ELEVENLABS_API_KEY"
	minRedacted = 8 // shorter values are too likely to be ordinary words
)

// secretName: env names that hold credentials by convention. Their values are redacted (they stay
// in the env when the runtime does not own them: skill scripts read e.g. a CRM token from it).
var secretName = regexp.MustCompile(`(?i)(TOKEN|SECRET|PASSWORD|PASSWD|API_KEY|_KEY|PRIVATE)`)

// Secrets is the runtime's own credentials.
type Secrets struct {
	values map[string]string
	owned  []string
	shared []string // owned, but skills read them too: never scrubbed
}

// Owned are the env names the runtime reads for cfg: its API key, the core key, the Slack tokens,
// each model endpoint's key, each http_secrets env (Luau never sees those) and, when voice notes
// are transcribed, the speech-to-text key (then out of the terminal's env).
func Owned(cfg *wire.Config) []string {
	names := []string{APIKey, CoreKey, SlackBot, SlackApp, LearningKey}
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
	if cfg.STT != nil && cfg.STT.Enabled {
		add(STTKey)
	}
	if cfg.Web != nil && cfg.Web.Search != nil {
		add(wire.SearchKeyEnv(cfg.Web.Search.Provider))
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
	// An http_secrets env is usually a skill's own token (a CRM key its bash scripts read from
	// the env): Go injects it into Luau's requests, and the terminal keeps it. It is redacted.
	for _, h := range cfg.Luau.HTTPSecrets {
		if h.Env != LearningKey {
			s.shared = append(s.shared, h.Env)
		}
	}
	for _, n := range s.owned {
		if v := env[n]; v != "" {
			s.values[n] = v
		}
	}
	// Owner feedback authority must never share a value with credentials the agent
	// or another service receives. Disable this optional surface on accidental reuse.
	if owner := s.values[LearningKey]; owner != "" {
		for name, value := range env {
			if name != LearningKey && value == owner {
				s.values[LearningKey] = ""
				break
			}
		}
	}
	return s
}

// Get is an owned secret's value ("" when unset).
func (s *Secrets) Get(name string) string { return s.values[name] }

// Scrub unsets the owned secrets from this process' env, except the passthrough allowlist and the
// http_secrets envs, so nothing the runtime starts (the terminal) inherits them.
func (s *Secrets) Scrub() {
	for _, n := range s.owned {
		if !slices.Contains(Passthrough, n) && !slices.Contains(s.shared, n) {
			_ = os.Unsetenv(n)
		}
	}
}

// Clean is environ without the owned secrets Scrub unsets (the passthrough allowlist and the
// http_secrets envs stay): the terminal's env, given explicitly, so nothing that put a secret back
// into this process' env after boot reaches it.
func (s *Secrets) Clean(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if slices.Contains(s.owned, k) && !slices.Contains(Passthrough, k) && !slices.Contains(s.shared, k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Redactor masks known secret values. It is safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values []string // as added
	forms  []string // each value and its JSON-escaped forms, longest first, so a value containing another is masked whole
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

// build masks each value as it is and as JSON writes it inside a string (with and without HTML
// escaping), so text that is already JSON (a tool call's arguments, an encoded result) is masked
// too: a value with a quote, a backslash, &, <, > or a control character differs once escaped.
func (r *Redactor) build() {
	r.forms = r.forms[:0]
	for _, v := range r.values {
		for _, f := range []string{v, jsonForm(v, true), jsonForm(v, false)} {
			if f != "" && !slices.Contains(r.forms, f) {
				r.forms = append(r.forms, f)
			}
		}
	}
	sort.SliceStable(r.forms, func(i, j int) bool { return len(r.forms[i]) > len(r.forms[j]) })
	pairs := make([]string, 0, 2*len(r.forms))
	for _, f := range r.forms {
		pairs = append(pairs, f, Mask)
	}
	r.r = strings.NewReplacer(pairs...)
}

// jsonForm is v as it appears inside a JSON string.
func jsonForm(v string, escapeHTML bool) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(escapeHTML)
	if enc.Encode(v) != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(b.String(), `"`), "\n"), `"`)
}

// Longest is the length of the longest text Redact masks (0: none): a writer that cuts text must
// keep that much around a cut to see a value whole.
func (r *Redactor) Longest() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.forms) == 0 {
		return 0
	}
	return len(r.forms[0])
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
