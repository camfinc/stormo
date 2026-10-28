package web

import (
	"html"
	"mime"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Bounds of what a page gives the model.
const (
	// MaxText is the most of a page's text returned (bytes, cut at a character).
	MaxText = 40_000
	// MaxLinks is the most links returned.
	MaxLinks = 100
	maxTitle = 300
	maxLabel = 200
)

// Page is a page's readable parts.
type Page struct {
	Title string `json:"title,omitempty"`
	Text  string `json:"text"`
	Links []Link `json:"links,omitempty"`
	// Truncated: the text was cut at MaxText.
	Truncated bool `json:"truncated,omitempty"`
}

// Link is one link of the page's main text: its label and absolute http(s) URL.
type Link struct {
	Text string `json:"text,omitempty"`
	URL  string `json:"url"`
}

// skipped are elements whose content is not the page's text (code, styling, navigation, forms,
// embedded media).
var skipped = map[string]bool{"script": true, "style": true, "noscript": true, "svg": true, "template": true, "iframe": true,
	"object": true, "canvas": true, "nav": true, "footer": true, "aside": true, "form": true, "select": true, "button": true,
	"head": true, "math": true, "video": true, "audio": true}

// blocks are elements that start a new line.
var blocks = map[string]bool{"p": true, "div": true, "br": true, "li": true, "ul": true, "ol": true, "tr": true, "table": true,
	"section": true, "article": true, "main": true, "header": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "blockquote": true, "pre": true, "hr": true, "dt": true, "dd": true, "dl": true, "figure": true, "figcaption": true,
	"td": true, "th": true, "address": true, "details": true, "summary": true}

var hrefRe = regexp.MustCompile(`(?is)(?:^|\s)href\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`)

// Extract is a page's readable text: for HTML its title, the text of its main content (the
// <main>, else the <article>, else the <body>; scripts, styles, navigation, forms and the like
// left out; headings as "#", list items as "-") and the links in it; any other text as it is.
// A tolerant scan with the standard library only (no HTML parser module): odd markup gives odd
// text, never an error. The body is read in its declared charset when that is Latin-1, else as
// UTF-8 (invalid bytes replaced).
func Extract(body []byte, contentType string, base *url.URL) Page {
	text := decode(body, contentType)
	mt, _, _ := mime.ParseMediaType(contentType)
	if mt != "text/html" && mt != "application/xhtml+xml" && !(mt == "" && looksHTML(text)) {
		t, cut := bound(strings.TrimSpace(text), MaxText)
		return Page{Text: t, Truncated: cut}
	}
	p := Page{Title: title(text)}
	w := &walker{base: base, seen: map[string]bool{}}
	w.walk(region(text))
	p.Text, p.Truncated = bound(w.result(), MaxText)
	p.Links = w.links
	return p
}

func looksHTML(s string) bool {
	head := strings.ToLower(s[:min(len(s), 512)])
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

// decode is body as UTF-8 text: Latin-1 (and the charsets that are it for text) mapped byte by
// byte, anything else read as UTF-8 with invalid bytes replaced.
func decode(body []byte, contentType string) string {
	_, params, _ := mime.ParseMediaType(contentType)
	switch strings.ToLower(params["charset"]) {
	case "iso-8859-1", "latin1", "latin-1", "iso8859-1", "windows-1252", "cp1252":
		var b strings.Builder
		for _, c := range body {
			b.WriteRune(rune(c))
		}
		return b.String()
	}
	if utf8.Valid(body) {
		return string(body)
	}
	return strings.ToValidUTF8(string(body), "�")
}

// bound cuts s to at most n bytes at a character.
func bound(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n], true
}

// title is the page's <title>, unescaped, on one line.
func title(s string) string {
	lower := strings.ToLower(s)
	i := strings.Index(lower, "<title")
	if i < 0 {
		return ""
	}
	open := strings.IndexByte(lower[i:], '>')
	if open < 0 {
		return ""
	}
	start := i + open + 1
	end := strings.Index(lower[start:], "</title")
	if end < 0 {
		return ""
	}
	t, _ := bound(collapse(html.UnescapeString(collapse(s[start:start+end]))), maxTitle)
	return strings.TrimSpace(t)
}

// region is the part of the page its main text is in: <main>…</main>, else the first <article> to
// the last </article>, else the <body>, else the whole.
func region(s string) string {
	lower := strings.ToLower(s)
	for _, tag := range []string{"main", "article", "body"} {
		i := indexTag(lower, tag)
		if i < 0 {
			continue
		}
		if j := strings.LastIndex(lower, "</"+tag); j > i {
			return s[i:j]
		}
		return s[i:]
	}
	return s
}

// indexTag is where the first <tag (followed by > or a space) starts, or -1.
func indexTag(lower, tag string) int {
	from := 0
	for {
		i := strings.Index(lower[from:], "<"+tag)
		if i < 0 {
			return -1
		}
		i += from
		if k := i + 1 + len(tag); k >= len(lower) || strings.ContainsRune("> \t\r\n/", rune(lower[k])) {
			return i
		}
		from = i + 1
	}
}

// collapse turns every run of ASCII whitespace into one space (a no-break space is kept).
func collapse(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\f' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	if space && b.Len() > 0 {
		b.WriteByte(' ')
	}
	return b.String()
}

// walker reads the text and links of a page's region.
type walker struct {
	base  *url.URL
	out   strings.Builder
	links []Link
	seen  map[string]bool
	href  string // the open <a>'s URL
	label int    // where its text starts in out
}

func (w *walker) newline() { w.out.WriteByte('\n') }

func (w *walker) text(raw string) {
	if w.out.Len() > 4*MaxText {
		return // enough: the rest would be cut
	}
	w.out.WriteString(html.UnescapeString(collapse(raw)))
}

func (w *walker) walk(s string) {
	lower := strings.ToLower(s)
	i := 0
	for i < len(s) {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			w.text(s[i:])
			return
		}
		w.text(s[i : i+lt])
		i += lt
		switch {
		case strings.HasPrefix(s[i:], "<!--"):
			end := strings.Index(s[i+4:], "-->")
			if end < 0 {
				return
			}
			i += 4 + end + 3
			continue
		case strings.HasPrefix(s[i:], "<!"), strings.HasPrefix(s[i:], "<?"):
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				return
			}
			i += end + 1
			continue
		}
		closing := strings.HasPrefix(s[i:], "</")
		n := i + 1
		if closing {
			n++
		}
		start := n
		for n < len(s) && (s[n] >= 'a' && s[n] <= 'z' || s[n] >= 'A' && s[n] <= 'Z' || s[n] >= '0' && s[n] <= '9' || s[n] == '-') {
			n++
		}
		if n == start {
			w.text("<") // not a tag: a lone "<"
			i++
			continue
		}
		name := lower[start:n]
		end := tagEnd(s, n)
		if end < 0 {
			return
		}
		attrs := s[n:end]
		i = end + 1
		if !closing && skipped[name] && !strings.HasSuffix(strings.TrimSpace(attrs), "/") {
			c := strings.Index(lower[i:], "</"+name)
			if c < 0 {
				return
			}
			i += c
			if e := strings.IndexByte(s[i:], '>'); e >= 0 {
				i += e + 1
			} else {
				return
			}
			continue
		}
		w.tag(name, closing, attrs)
	}
}

// tagEnd is the index of the '>' closing a tag whose name ends at n (quoted attribute values
// may hold one), or -1.
func tagEnd(s string, n int) int {
	var quote byte
	for k := n; k < len(s); k++ {
		c := s[k]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return k
		}
	}
	return -1
}

func (w *walker) tag(name string, closing bool, attrs string) {
	switch {
	case name == "a" && !closing:
		w.href, w.label = "", w.out.Len()
		if m := hrefRe.FindStringSubmatch(attrs); m != nil {
			w.href = html.UnescapeString(strings.TrimSpace(m[1] + m[2] + m[3]))
		}
	case name == "a":
		w.link(w.href, w.out.String()[min(w.label, w.out.Len()):])
		w.href = ""
	case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
		w.newline()
		if !closing {
			w.out.WriteString(strings.Repeat("#", int(name[1]-'0')) + " ")
		}
	case name == "li" && !closing:
		w.newline()
		w.out.WriteString("- ")
	case blocks[name]:
		w.newline()
	}
}

func (w *walker) link(href, label string) {
	if href == "" || len(w.links) >= MaxLinks || w.base == nil {
		return
	}
	u, err := w.base.Parse(href)
	if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return
	}
	u.Fragment = ""
	if w.seen[u.String()] {
		return
	}
	w.seen[u.String()] = true
	label, _ = bound(strings.TrimSpace(collapse(label)), maxLabel)
	w.links = append(w.links, Link{Text: label, URL: u.String()})
}

// result is the text read, one trimmed line per block, no empty lines.
func (w *walker) result() string {
	lines := strings.Split(w.out.String(), "\n")
	out := lines[:0]
	for _, l := range lines {
		if l = strings.Trim(l, " "); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
