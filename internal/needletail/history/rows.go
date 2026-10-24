package history

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

func sprintf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

// What the runtime writes into a turn's input besides what people wrote (the Slack gateway's
// header and blocks, a follow-up's frame, recalled memory) is left out of what is indexed and
// shown: a search for "Slack message" must not find every row, and a hit shows what was said.
var (
	recallBlock = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(strings.TrimSuffix(memory.RecallHeader, "\n")) + `.*?` + regexp.QuoteMeta(memory.RecallFooter) + `\s*`)
	slackHeader = regexp.MustCompile(`^\[Slack message from (.*?)( — overheard in a channel you listen to[^\]]*)?\]( \[unverified[^\]]*\])?\s*$`)
)

// userText is a user row as people wrote it.
type userText struct {
	text       string
	from       string // who, as the Slack header names them ("Ana (U…)"; several, comma-joined)
	overheard  bool   // heard in a channel the agent listens to, not addressed to it
	unverified bool   // from someone allowed_users does not list
	followUp   bool   // a follow-up's input: the agent's own words, no one wrote it now
}

// cleanUser strips the runtime's markers from a user row: the Slack gateway's header lines
// ([Slack message from …], whose quoted look-alikes the gateway neutralises), its thread
// context's frame (the quoted lines stay) and its handler notes, a follow-up's frame and any
// recalled-memory block.
func cleanUser(content string) userText {
	var u userText
	content = recallBlock.ReplaceAllString(content, "")
	lines := strings.Split(content, "\n")
	var keep, from []string
	notes := false
	for i, l := range lines {
		switch {
		case i == 0 && strings.HasPrefix(l, "[Follow-up "):
			u.followUp = true
			continue
		case strings.HasPrefix(l, "[Thread context"), strings.HasPrefix(l, "[End of thread context]"):
			continue
		case strings.HasPrefix(l, "[Handler notes"):
			notes = true
			continue
		}
		if m := slackHeader.FindStringSubmatch(l); m != nil {
			notes = false
			if who := strings.TrimSpace(m[1]); who != "" && !contains(from, who) {
				from = append(from, who)
			}
			u.overheard = u.overheard || m[2] != ""
			u.unverified = u.unverified || m[3] != ""
			continue
		}
		if notes {
			continue // what the agent's own handlers noted
		}
		keep = append(keep, l)
	}
	u.text = strings.TrimSpace(strings.Join(keep, "\n"))
	u.from = strings.Join(from, ", ")
	return u
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// silent are replies that say nothing (the scheduler's and the gateway's tokens, a turn with no
// answer).
func silent(s string) bool {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "[SILENT]", "SILENT", "NO_REPLY", "NO REPLY", "(NO ANSWER)":
		return true
	}
	return false
}

// indexText is what of m is indexed: a person's words (not a follow-up's input), the agent's
// replies (not a silent one); never a summary, tool output or tool arguments.
func indexText(m wire.Message) (string, bool) {
	switch m.Role {
	case "user":
		if m.Kind == wire.KindFollowUp {
			return "", false
		}
		u := cleanUser(m.Content)
		if u.followUp || u.text == "" {
			return "", false
		}
		return u.text, true
	case "assistant":
		if silent(m.Content) {
			return "", false
		}
		return m.Content, true
	}
	return "", false
}

// preview is a conversation's first person's words, on one line, at most 120 characters.
func preview(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}

// clip is s within n bytes (on a character boundary), and whether it was cut.
func clip(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…", true
}

// matcher finds a query's words in text, as the index folded them.
type matcher struct {
	terms    map[string]bool
	prefixes []string
}

func newMatcher(terms []string) matcher {
	m := matcher{terms: map[string]bool{}}
	for _, t := range terms {
		if p, ok := strings.CutSuffix(t, "*"); ok {
			m.prefixes = append(m.prefixes, p)
		} else {
			m.terms[t] = true
		}
	}
	return m
}

func (m matcher) match(w string) bool {
	if m.terms[w] {
		return true
	}
	for _, p := range m.prefixes {
		if strings.HasPrefix(w, p) {
			return true
		}
	}
	return false
}

// words calls f with each word of text (letters and digits, as the tokenizer splits) folded, and
// its byte offset; f returns false to stop.
func eachWord(text string, f func(word string, at int) bool) {
	start := -1
	for i, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			if !f(memory.Fold(text[start:i]), start) {
				return
			}
			start = -1
		}
	}
	if start >= 0 {
		f(memory.Fold(text[start:]), start)
	}
}

// rowScore is how well text answers the query: the idf of each distinct query term it holds;
// and whether it holds every phrase.
func rowScore(text string, m matcher, idf map[string]float64, phrases []string) float64 {
	if len(phrases) > 0 {
		joined := " " + strings.Join(memory.Words(text), " ") + " "
		for _, p := range phrases {
			if !strings.Contains(joined, " "+p+" ") {
				return 0
			}
		}
	}
	seen := map[string]bool{}
	var score float64
	eachWord(text, func(w string, _ int) bool {
		if !seen[w] && m.match(w) {
			seen[w] = true
			if v, ok := idf[w]; ok {
				score += v
			} else {
				score += 0.5 // a prefix's expansion that the index does not hold any more
			}
		}
		return true
	})
	return score
}

// snippet is the part of text around its first matched word, on one line, at most about 300
// bytes.
func snippet(text string, m matcher) string {
	at := -1
	eachWord(text, func(w string, i int) bool {
		if m.match(w) {
			at = i
			return false
		}
		return true
	})
	if at < 0 {
		at = 0
	}
	start, end := max(0, at-100), min(len(text), at+200)
	for start > 0 && !utf8.RuneStart(text[start]) {
		start--
	}
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}
