package memory

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// isSpace is the trim set (pkg/learning's unicodeSpace), whose runs Normalize collapses.
func isSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00a0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Normalize is pkg/learning.Normalize (a copy, parity-tested), without its regexp: lower case,
// every run of spaces one space, trimmed. Two entries with the same Normalize are the same memory
// to the runtime, the merge at boot and the ledger.
func Normalize(entry string) string {
	s := strings.ToLower(entry)
	var b strings.Builder
	b.Grow(len(s))
	pending := false
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if isSpace(r) && !(r == utf8.RuneError && n == 1) {
			pending = true
		} else {
			if pending && b.Len() > 0 {
				b.WriteByte(' ')
			}
			pending = false
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// fold is s lower-cased with diacritics and Latin ligatures folded (cotización → cotizacion,
// straße → strasse), as FTS5's remove_diacritics 2 matches them.
func fold(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if f, ok := folds[r]; ok {
			b.WriteString(f)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var folds = func() map[rune]string {
	m := map[rune]string{'ß': "ss", 'æ': "ae", 'œ': "oe", 'ø': "o", 'đ': "d", 'ð': "d", 'þ': "th", 'ł': "l", 'ı': "i", 'ŀ': "l", 'ﬁ': "fi", 'ﬂ': "fl", 'ﬀ': "ff"}
	for base, runes := range map[string]string{
		"a": "àáâãäåāăąǎȁȃạảấầẩẫậắằẳẵặ",
		"c": "çćĉċč",
		"d": "ď",
		"e": "èéêëēĕėęěȅȇẹẻẽếềểễệ",
		"g": "ĝğġģ",
		"h": "ĥħ",
		"i": "ìíîïĩīĭįǐȉȋỉị",
		"j": "ĵ",
		"k": "ķ",
		"l": "ĺļľ",
		"n": "ñńņňǹ",
		"o": "òóôõöōŏőǒȍȏọỏốồổỗộớờởỡợ",
		"r": "ŕŗř",
		"s": "śŝşšș",
		"t": "ţťț",
		"u": "ùúûüũūŭůűųǔȕȗụủứừửữự",
		"w": "ŵ",
		"y": "ýÿŷỳỵỷỹ",
		"z": "źżž",
	} {
		for _, r := range runes {
			m[r] = base
		}
	}
	return m
}()

// tokens is s's index terms: folded, split at anything not a letter or digit, at least two
// characters, stop words dropped.
func tokens(s string) []string {
	out := []string{}
	for _, t := range words(s) {
		if !stop[t] {
			out = append(out, t)
		}
	}
	return out
}

// words is s folded and split, words of one character dropped, stop words kept.
func words(s string) []string {
	f := fold(s)
	out := make([]string, 0, len(f)/6+1)
	start := -1
	for i, r := range f {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			if w := f[start:i]; len(w) > 1 && utf8.RuneCountInString(w) >= 2 {
				out = append(out, w)
			}
			start = -1
		}
	}
	if start >= 0 {
		if w := f[start:]; len(w) > 1 && utf8.RuneCountInString(w) >= 2 {
			out = append(out, w)
		}
	}
	return out
}

// stop are Spanish and English stop words, dropped from the index and from queries.
var stop = func() map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(`
a al algo algun alguna algunas alguno algunos ante antes aqui asi aun cada como con contra cual cuales cuando de del desde donde dos el ella ellas ello ellos en entre era eran es esa esas ese eso esos esta estaba estaban estan estar este esto estos fue fueron ha habia han hasta hay la las le les lo los mas me mi mis mientras muy nada ni no nos nosotros o os otra otras otro otros para pero poco por porque que quien quienes se sea sean ser si sido sin sobre su sus tambien tan tanto te tiene tienen todo todos tu tus un una uno unos usted ustedes ya yo
about above after again all also am an and any are as at be been before being below between both but by can could did do does doing down during each few for from further had has have having he her here hers him his how if in into is it its itself just me more most my no nor not now of off on once only or other our ours out over own same she should so some such than that the their theirs them then there these they this those through to too under until up very was we were what when where which while who whom why will with would you your yours`) {
		m[w] = true
	}
	return m
}()

// distinct is the set of xs.
func distinct(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// ---- PII flags ------------------------------------------------------------------------------

// piiRules are pkg/learning's generic scrub rules (a copy, parity-tested), in its order: each
// rule's matches are replaced before the next looks, so a card number is not also a phone.
var piiRules = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"secret", regexp.MustCompile(`\b(?:sk|pk|rk|ghp|gho|xox[abpr])_[A-Za-z0-9_-]{10,}\b`)},
	{"secret", regexp.MustCompile(`\b\d{8,10}:AA[A-Za-z0-9_-]{30,}\b`)},
	{"secret", regexp.MustCompile(`\bBearer\s+[A-Za-z0-9._-]{16,}`)},
	{"email", regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)},
	{"card", regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)},
	{"phone", regexp.MustCompile(`(?:\+\d{1,3}[\s.-]?)?\(?\d{2,4}\)?[\s.-]?\d{3,4}[\s.-]?\d{3,4}\b`)},
}

// PII is the kinds of personal data the generic rules find in text (sorted): informational,
// shown with the record; the dream still scrubs hot entries with the instance's full rules.
func PII(text string) []string {
	found := map[string]bool{}
	for _, r := range piiRules {
		text = r.re.ReplaceAllStringFunc(text, func(string) string {
			found[r.kind] = true
			return "[" + strings.ToUpper(r.kind) + "]"
		})
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---- checks ---------------------------------------------------------------------------------

// MaxText is a record's longest text saved through memory_manage or Luau (UTF-16 units). The
// memory tool's hot entries are bounded by their file's budget instead, as in Hermes.
const MaxText = 1000

// Errors of the checks.
var (
	ErrInvisible = errors.New("the text has invisible or direction-changing characters (zero-width, bidi controls): write it plainly")
	ErrControl   = errors.New("the text has control characters (only newlines and tabs are allowed)")
	ErrInjection = errors.New("the text reads as instructions to override your rules or reveal secrets; a memory is a note, not a command: rephrase it as a fact")
)

// invisible are characters that hide or reorder text: zero-width, joiners and bidi controls.
func invisible(r rune) bool {
	return r >= 0x200b && r <= 0x200f || r >= 0x202a && r <= 0x202e || r >= 0x2060 && r <= 0x2064 || r >= 0x2066 && r <= 0x2069
}

// injection is the heuristic Hermes applies to what can enter the system prompt: text telling
// the model to drop its instructions or hand out what it holds.
var injection = regexp.MustCompile(`(?i)(ignore|disregard|forget|override)\s+(all\s+|any\s+)?(of\s+)?(the\s+|your\s+|my\s+)?(previous|prior|above|earlier|system)\s+(instructions|rules|prompts?|messages)` +
	`|(reveal|print|show|send|leak|exfiltrate)\s+(me\s+)?(your\s+|the\s+)?(system\s+prompt|api\s+keys?|secrets?|credentials|tokens?|passwords?)` +
	`|(ignora|olvida|omite)\s+(todas\s+)?(las\s+|tus\s+)?(instrucciones|reglas)\s+(anteriores|previas|del\s+sistema)` +
	`|(revela|muestra|envía|envia)\s+(tu\s+|el\s+|los\s+|las\s+)?(prompt\s+del\s+sistema|claves?|secretos?|credenciales|tokens?|contraseñas?)`)

// checkText is what every new text must pass: invisible characters, and for archive records
// (strict) control characters and the length cap.
func checkText(text string, strict bool) error {
	for _, r := range text {
		if invisible(r) {
			return ErrInvisible
		}
		if strict && (r < 0x20 && r != '\n' && r != '\t' || r == 0x7f || r >= 0x80 && r <= 0x9f) {
			return ErrControl
		}
	}
	if strict && Units(text) > MaxText {
		return fmt.Errorf("the text is %d UTF-16 units, over %d: split it into several records", Units(text), MaxText)
	}
	return nil
}

// checkHot is the injection heuristic for text that becomes hot (in every system prompt).
func checkHot(text string) error {
	if injection.MatchString(text) {
		return ErrInjection
	}
	return nil
}

// ---- tags -----------------------------------------------------------------------------------

// MaxTags is a record's most tags.
const MaxTags = 8

var tagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// cleanTags is tags lower-cased and folded, deduplicated in order, each checked.
func cleanTags(tags []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		t = strings.TrimSpace(fold(t))
		if t == "" || seen[t] {
			continue
		}
		if !tagRe.MatchString(t) {
			return nil, fmt.Errorf("tag %q: tags are 1-32 characters, a-z, 0-9, _ and -, starting with a letter or digit", t)
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > MaxTags {
		return nil, fmt.Errorf("%d tags: at most %d", len(out), MaxTags)
	}
	return out, nil
}
