package luau

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
)

// The http_secrets values are the credentials a server could echo back to a script. Besides the
// runtime's redactor (Runtime.Redact: values of 8 bytes or more, as they are and as Go's JSON
// writes them), every answer, log and result of a run has each of them masked whatever its length,
// in any form a JSON string can spell it: each character as itself or as \uXXXX (either hex case,
// a pair of surrogates past U+FFFF), and the short escapes (\/ for "/", \" \\ \b \f \n \r \t).
// json_decode would turn any of those back into the value. A server that encodes it some other way
// (base64, percent-encoding) can still hand it over: http_secrets are for hosts that do not echo.

type secretMasker struct {
	key string
	re  *regexp.Regexp
}

// secretValues are the nonempty http_secrets values, longest first.
func (rt *Runtime) secretValues() []string {
	if rt.Secret == nil {
		return nil
	}
	var out []string
	for _, s := range rt.HTTPSecrets {
		if v := rt.Secret(s.Env); v != "" && !contains(out, v) {
			out = append(out, v)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// masker is the matcher of the current values (built again only when they change).
func (rt *Runtime) masker() *regexp.Regexp {
	values := rt.secretValues()
	if len(values) == 0 {
		return nil
	}
	key := strings.Join(values, "\x00")
	rt.maskMu.Lock()
	defer rt.maskMu.Unlock()
	if rt.mask.re != nil && rt.mask.key == key {
		return rt.mask.re
	}
	alts := make([]string, len(values))
	for i, v := range values {
		alts[i] = escapedForms(v)
	}
	rt.mask = secretMasker{key: key, re: regexp.MustCompile(strings.Join(alts, "|"))}
	return rt.mask.re
}

// escapedForms is a pattern matching v with each character in any of its JSON string spellings.
func escapedForms(v string) string {
	var b strings.Builder
	for _, r := range v {
		forms := []string{regexp.QuoteMeta(string(r))}
		if r > 0xffff {
			hi, lo := utf16.EncodeRune(r)
			forms = append(forms, `\\u`+hexForm(hi)+`\\u`+hexForm(lo))
		} else {
			forms = append(forms, `\\u`+hexForm(r))
		}
		if short, ok := map[rune]string{'/': `\\/`, '"': `\\"`, '\\': `\\\\`, '\b': `\\b`, '\f': `\\f`, '\n': `\\n`, '\r': `\\r`, '\t': `\\t`}[r]; ok {
			forms = append(forms, short)
		}
		b.WriteString("(?:" + strings.Join(forms, "|") + ")")
	}
	return b.String()
}

// hexForm is r's four hex digits, each letter in either case.
func hexForm(r rune) string {
	var b strings.Builder
	for _, c := range fmt.Sprintf("%04x", r) {
		if c >= 'a' && c <= 'f' {
			b.WriteString("[" + string(c) + strings.ToUpper(string(c)) + "]")
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// maskSecrets masks the http_secrets values in s (any of their JSON spellings).
func (rt *Runtime) maskSecrets(s string) string {
	if re := rt.masker(); re != nil {
		return re.ReplaceAllLiteralString(s, mask)
	}
	return s
}

const mask = "[REDACTED]"

// scrub is v with every string in it redacted, before the answer is encoded: a value spelled with
// JSON escapes inside a string (a body, a file, a core answer's text) is escaped once more by the
// encoding, where no pass over the encoded answer would see it.
func (rt *Runtime) scrub(v any) any {
	switch x := v.(type) {
	case string:
		return rt.redact(x)
	case json.RawMessage:
		return json.RawMessage(rt.redact(string(x)))
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, s := range x {
			out[rt.redact(k)] = rt.redact(s)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[rt.redact(k)] = rt.scrub(e)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(x))
		for i, e := range x {
			out[i] = rt.scrub(e).(map[string]any)
		}
		return out
	case []string:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = rt.redact(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = rt.scrub(e)
		}
		return out
	}
	return v
}
