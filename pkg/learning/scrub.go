package learning

import (
	"regexp"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/instance"
)

// PII scrubber for anything that crosses from runtime (S3) into git. Deliberately over-eager: a
// redacted learning can be rewritten by a human, a leaked identity number cannot be un-committed.

type scrubRule struct {
	kind    string
	re      *regexp.Regexp
	require []*regexp.Regexp // every one must also match the hit (RE2 has no lookaheads)
}

// Scrubber redacts with the engine's generic rules plus the instance's token prefixes and rules.
type Scrubber struct{ rules []scrubRule }

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// NewScrubber builds the rules for an instance (stormo.yaml scrub: token_prefixes, rules). The
// instance's patterns were validated when it loaded.
func NewScrubber(inst *instance.Instance) *Scrubber {
	prefixes := []string{}
	for _, p := range inst.ScrubTokenPrefixes {
		prefixes = append(prefixes, nonAlnum.ReplaceAllString(p, ""))
	}
	prefixes = append(prefixes, "sk", "pk", "rk", "ghp", "gho", "xox[abpr]")
	rules := []scrubRule{
		{kind: "secret", re: regexp.MustCompile(`\b(?:` + strings.Join(prefixes, "|") + `)_[A-Za-z0-9_-]{10,}\b`)},
		{kind: "secret", re: regexp.MustCompile(`\b\d{8,10}:AA[A-Za-z0-9_-]{30,}\b`)}, // Telegram bot token
		{kind: "secret", re: regexp.MustCompile(`\bBearer\s+[A-Za-z0-9._-]{16,}`)},
		{kind: "email", re: regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)},
		{kind: "card", re: regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)},
		{kind: "phone", re: regexp.MustCompile(`(?:\+\d{1,3}[\s.-]?)?\(?\d{2,4}\)?[\s.-]?\d{3,4}[\s.-]?\d{3,4}\b`)},
	}
	for _, r := range inst.ScrubRules {
		rule := scrubRule{kind: r.Kind, re: regexp.MustCompile(r.Pattern)}
		for _, q := range r.Require {
			rule.require = append(rule.require, regexp.MustCompile(q))
		}
		rules = append(rules, rule)
	}
	return &Scrubber{rules: rules}
}

// Scrub returns the redacted text and the sorted finding kinds.
func (s *Scrubber) Scrub(input string) (string, []string) {
	text := input
	found := map[string]bool{}
	for _, r := range s.rules {
		text = r.re.ReplaceAllStringFunc(text, func(m string) string {
			for _, q := range r.require {
				if !q.MatchString(m) {
					return m
				}
			}
			found[r.kind] = true
			return "[" + strings.ToUpper(r.kind) + "]"
		})
	}
	kinds := make([]string, 0, len(found))
	for k := range found {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return text, kinds
}
