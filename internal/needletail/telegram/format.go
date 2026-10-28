package telegram

import (
	"regexp"
	"strings"
	"unicode/utf16"
)

// Telegram's text: a message carries at most 4096 characters after its entities are parsed. The
// model writes Markdown; it is cut into parts of at most MaxPart UTF-16 units of source (a code
// block closed at the cut and reopened in the next part), and each part converted to MarkdownV2
// (bold, italic, strike, inline code, code blocks, http(s) and mailto links; everything else
// escaped). A part Telegram refuses to parse is sent again as plain text.

// MaxMessage is the most characters one message carries; MaxPart the most source a part takes
// (the conversion only removes characters, but a plain resend keeps them all).
const (
	MaxMessage = 4096
	MaxPart    = 4000
)

// units is s's length in UTF-16 code units (what Telegram counts).
func units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// Split cuts Markdown into parts of at most limit UTF-16 units, at line ends where it can; a code
// block open at a cut is closed there and reopened (with its language) in the next part.
func Split(text string, limit int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if units(text) <= limit {
		return []string{text}
	}
	var parts []string
	var cur strings.Builder
	fence := "" // the open code block's fence line (```lang), "" outside one
	closed := func() string {
		s := cur.String()
		if fence != "" {
			s = strings.TrimRight(s, "\n") + "\n```"
		}
		return strings.TrimRight(s, "\n")
	}
	emit := func() {
		if s := closed(); strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
		cur.Reset()
		if fence != "" {
			cur.WriteString(fence + "\n")
		}
	}
	room := func() int { return limit - units(cur.String()) - 4 } // 4: a closing fence
	for _, line := range strings.SplitAfter(text, "\n") {
		for units(line) > room() {
			if cur.Len() > len(fence)+1 {
				emit() // the part is full: the line goes on in the next
				continue
			}
			cut, n := 0, 0 // a line longer than a part: cut inside it
			for i, r := range line {
				if n+utf16.RuneLen(r) > room() && i > 0 {
					break
				}
				n += utf16.RuneLen(r)
				cut = i + len(string(r))
			}
			cur.WriteString(line[:cut])
			line = line[cut:]
			emit()
		}
		cur.WriteString(line)
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") {
			if fence == "" {
				fence = "```" + onlyWord(strings.TrimPrefix(t, "```"))
			} else {
				fence = ""
			}
		}
	}
	if s := closed(); strings.TrimSpace(s) != "" {
		parts = append(parts, s)
	}
	return parts
}

// mdv2Special are the characters MarkdownV2 wants escaped in text.
const mdv2Special = "_*[]()~`>#+-=|{}.!\\"

// esc escapes text for MarkdownV2.
func esc(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(mdv2Special, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// escCode escapes code (inline or a block): only ` and \.
func escCode(s string) string {
	return strings.NewReplacer("\\", "\\\\", "`", "\\`").Replace(s)
}

var (
	inlineRe = regexp.MustCompile("`([^`\n]+)`")
	linkRe   = regexp.MustCompile(`\[([^\]\n]+)\]\(((?:https?|mailto):[^\s()]+)\)`)
	boldRe   = regexp.MustCompile(`\*\*([^*\n]+)\*\*|__([^_\n]+)__`)
	strikeRe = regexp.MustCompile(`~~([^~\n]+)~~`)
	italicRe = regexp.MustCompile(`(^|[\s(])[*_]([^\s*_][^*_\n]*?)[*_]([\s).,!?:;]|$)`)
	headRe   = regexp.MustCompile(`^#{1,6}\s+(.*)$`)
	bulletRe = regexp.MustCompile(`^(\s*)[-*+]\s+`)
)

// Format is Markdown as MarkdownV2.
func Format(md string) string {
	var out []string
	inCode := false
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			if !inCode {
				out = append(out, "```"+onlyWord(strings.TrimPrefix(t, "```")))
			} else {
				out = append(out, "```")
			}
			inCode = !inCode
			continue
		}
		if inCode {
			out = append(out, escCode(line))
			continue
		}
		if m := headRe.FindStringSubmatch(t); m != nil {
			out = append(out, "*"+inline(m[1])+"*")
			continue
		}
		if m := bulletRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1]+"• "+inline(line[len(m[0]):]))
			continue
		}
		out = append(out, inline(line))
	}
	if inCode {
		out = append(out, "```")
	}
	return strings.Join(out, "\n")
}

// onlyWord keeps a code block's language when it is a plain word.
func onlyWord(s string) string {
	s = strings.TrimSpace(s)
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '-' || r == '_') {
			return ""
		}
	}
	return s
}

// inline converts one line outside code blocks: spans are found in order of precedence (inline
// code, links, bold, strike, italic), each converted with its text escaped; the rest escaped.
func inline(s string) string {
	type span struct {
		start, end int
		out        string
	}
	var spans []span
	taken := func(a, b int) bool {
		for _, sp := range spans {
			if a < sp.end && b > sp.start {
				return true
			}
		}
		return false
	}
	add := func(re *regexp.Regexp, conv func(m []string) string, group int) {
		for _, idx := range re.FindAllStringSubmatchIndex(s, -1) {
			a, b := idx[0], idx[1]
			if group > 0 { // italic: what surrounds the delimiters is not part of the span
				a, b = idx[3], idx[6]
			}
			if taken(a, b) {
				continue
			}
			m := []string{}
			for i := 0; i < len(idx); i += 2 {
				if idx[i] < 0 {
					m = append(m, "")
				} else {
					m = append(m, s[idx[i]:idx[i+1]])
				}
			}
			spans = append(spans, span{a, b, conv(m)})
		}
	}
	add(inlineRe, func(m []string) string { return "`" + escCode(m[1]) + "`" }, 0)
	add(linkRe, func(m []string) string {
		return "[" + esc(m[1]) + "](" + strings.NewReplacer("\\", "\\\\", ")", "\\)").Replace(m[2]) + ")"
	}, 0)
	add(boldRe, func(m []string) string { return "*" + esc(m[1]+m[2]) + "*" }, 0)
	add(strikeRe, func(m []string) string { return "~" + esc(m[1]) + "~" }, 0)
	add(italicRe, func(m []string) string { return "_" + esc(m[2]) + "_" }, 1)
	// in order
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].start < spans[j-1].start; j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
	var b strings.Builder
	last := 0
	for _, sp := range spans {
		b.WriteString(esc(s[last:sp.start]))
		b.WriteString(sp.out)
		last = sp.end
	}
	b.WriteString(esc(s[last:]))
	return b.String()
}
