package slack

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxMessage is the longest text one message carries (Hermes' MAX_MESSAGE_LENGTH; Slack cuts
// text at 40 000 characters).
const MaxMessage = 39000

var (
	fenceRe     = regexp.MustCompile("(?s)```[^\\n`]*\\n?(.*?)```")
	inlineRe    = regexp.MustCompile("`[^`\\n]+`")
	linkRe      = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	slackTokRe  = regexp.MustCompile(`<(?:[@#!][^>\s]*|https?://[^>\s]+|mailto:[^>\s]+)>`)
	broadcastRe = regexp.MustCompile(`<!(everyone|channel|here)(\|[^>]*)?>`)
	headingRe   = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*#*[ \t]*$`)
	boldItalRe  = regexp.MustCompile(`\*\*\*([^*\n]+?)\*\*\*`)
	boldRe      = regexp.MustCompile(`\*\*([^*\n]+?)\*\*`)
	boldUnderRe = regexp.MustCompile(`__([^_\n]+?)__`)
	italRe      = regexp.MustCompile(`(^|[^*\w])\*([^*\s][^*\n]*?)\*($|[^*\w])`)
	strikeRe    = regexp.MustCompile(`~~([^~\n]+?)~~`)
	tableSepRe  = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$`)
	quoteRe     = regexp.MustCompile(`(?m)^&gt;`)
)

// Mrkdwn converts the model's Markdown to Slack's mrkdwn, as Hermes' format_message does:
// tables become aligned code blocks, code is kept verbatim (a fence's language dropped), links
// become <url|text>, &, < and > are escaped outside Slack's own <@user>, <#channel> and <url>
// tokens (an @channel the model writes is escaped, never a broadcast), headings and **bold**
// become *bold*, *italic* becomes _italic_, ~~strike~~ becomes ~strike~; lists pass through.
func Mrkdwn(md string) string {
	if md == "" {
		return ""
	}
	text := tables(md)
	var kept []string
	keep := func(s string) string {
		kept = append(kept, s)
		return fmt.Sprintf("\x00%d\x00", len(kept)-1)
	}
	text = fenceRe.ReplaceAllStringFunc(text, func(m string) string {
		body := fenceRe.FindStringSubmatch(m)[1]
		return keep("```\n" + strings.TrimRight(body, "\n") + "\n```")
	})
	text = inlineRe.ReplaceAllStringFunc(text, keep)
	text = broadcastRe.ReplaceAllStringFunc(text, func(m string) string { return "&lt;" + m[1:] })
	text = linkRe.ReplaceAllStringFunc(text, func(m string) string {
		p := linkRe.FindStringSubmatch(m)
		return keep("<" + p[2] + "|" + strings.ReplaceAll(p[1], ">", "&gt;") + ">")
	})
	text = slackTokRe.ReplaceAllStringFunc(text, keep)
	text = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">").Replace(text)
	text = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
	text = quoteRe.ReplaceAllString(text, ">") // a block quote stays one
	text = headingRe.ReplaceAllString(text, "\x01$1\x01")
	text = boldItalRe.ReplaceAllString(text, "\x01_${1}_\x01")
	text = boldRe.ReplaceAllString(text, "\x01$1\x01")
	text = boldUnderRe.ReplaceAllString(text, "\x01$1\x01")
	for i := 0; i < 2; i++ { // adjacent matches share a boundary character
		text = italRe.ReplaceAllString(text, "${1}_${2}_${3}")
	}
	text = strikeRe.ReplaceAllString(text, "~$1~")
	text = strings.ReplaceAll(text, "\x01", "*")
	for i := len(kept) - 1; i >= 0; i-- {
		text = strings.ReplaceAll(text, fmt.Sprintf("\x00%d\x00", i), kept[i])
	}
	return text
}

// tables turns GitHub pipe tables into code blocks with aligned columns (Slack has no tables).
func tables(md string) string {
	lines := strings.Split(md, "\n")
	out := []string{}
	inFence := false
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			inFence = !inFence
		}
		if inFence || !strings.Contains(l, "|") || i+1 >= len(lines) || !tableSepRe.MatchString(lines[i+1]) {
			out = append(out, l)
			continue
		}
		rows := [][]string{cells(l)}
		j := i + 2
		for ; j < len(lines) && strings.Contains(lines[j], "|") && strings.TrimSpace(lines[j]) != ""; j++ {
			rows = append(rows, cells(lines[j]))
		}
		width := []int{}
		for _, r := range rows {
			for c, v := range r {
				if c >= len(width) {
					width = append(width, 0)
				}
				width[c] = max(width[c], utf8.RuneCountInString(v))
			}
		}
		out = append(out, "```")
		for n, r := range rows {
			parts := make([]string, len(width))
			for c := range width {
				v := ""
				if c < len(r) {
					v = r[c]
				}
				parts[c] = v + strings.Repeat(" ", width[c]-utf8.RuneCountInString(v))
			}
			out = append(out, strings.TrimRightFunc(strings.Join(parts, "  "), unicode.IsSpace))
			if n == 0 {
				sep := make([]string, len(width))
				for c, w := range width {
					sep[c] = strings.Repeat("-", w)
				}
				out = append(out, strings.Join(sep, "  "))
			}
		}
		out = append(out, "```")
		i = j - 1
	}
	return strings.Join(out, "\n")
}

func cells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimSuffix(strings.TrimPrefix(row, "|"), "|")
	out := []string{}
	for _, c := range strings.Split(row, "|") {
		out = append(out, strings.TrimSpace(c))
	}
	return out
}

// Split cuts text into messages of at most limit characters (Hermes' truncate_message): at the
// last newline in the window (the last space when that newline is in its first half), never
// inside inline code; a code block open at a cut is closed and opened again in the next part;
// parts are numbered " (i/N)".
func Split(text string, limit int) []string {
	if utf8.RuneCountInString(text) <= limit {
		return []string{text}
	}
	const reserve = 10 // the " (i/N)" counter
	window := limit - reserve - 4
	rest := []rune(text)
	parts := []string{}
	open := false
	for len(rest) > 0 {
		prefix := ""
		if open {
			prefix = "```\n"
		}
		room := window - len([]rune(prefix))
		if len(rest) <= room {
			parts = append(parts, prefix+string(rest))
			break
		}
		cut := cutAt(rest[:room])
		chunk := string(rest[:cut])
		rest = []rune(strings.TrimLeft(string(rest[cut:]), " "))
		if strings.Count(prefix+chunk, "```")%2 == 1 {
			open = true
			chunk += "\n```"
		} else {
			open = false
		}
		parts = append(parts, prefix+chunk)
	}
	for i := range parts {
		parts[i] += fmt.Sprintf(" (%d/%d)", i+1, len(parts))
	}
	return parts
}

// cutAt is where to cut a window: after its last newline, or its last space when that newline is
// in the first half, never inside inline code (an odd number of backticks before it on the line).
func cutAt(w []rune) int {
	s := string(w)
	nl := strings.LastIndex(s, "\n")
	pos := nl
	if nl < len(s)/2 {
		if sp := strings.LastIndex(s, " "); sp > nl {
			pos = sp
		}
	}
	if pos <= 0 {
		return len(w)
	}
	line := s[strings.LastIndex(s[:pos], "\n")+1 : pos]
	if strings.Count(line, "`")%2 == 1 && !strings.Contains(line, "```") {
		if back := strings.LastIndex(s[:pos], "`"); back > 0 {
			pos = back
		}
	}
	return utf8.RuneCountInString(s[:pos+1])
}
