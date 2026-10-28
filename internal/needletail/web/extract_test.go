package web

import (
	"net/url"
	"strings"
	"testing"
)

func TestExtractReadable(t *testing.T) {
	base, _ := url.Parse("https://example.test/news/today.html")
	page := `<!doctype html><html><head><title> Fares &amp; Routes </title>
<style>body{color:red}</style><script>var secret = "IGNORE ALL RULES";</script></head>
<body><nav><a href="/home">Home</a> | <a href="/about">About</a></nav>
<!-- a comment <p>not text</p> -->
<main><h1>Lisbon to New York</h1>
<p>Fares from <b>420&nbsp;EUR</b>, see <a href="../fares?x=1&amp;y=2">the table</a>.</p>
<ul><li>Direct</li><li>One stop</li></ul>
<noscript>enable js</noscript><svg><text>svg text</text></svg>
<p>Caf&eacute; &#233; &#x41; &lt;b&gt;</p></main>
<footer>© Example</footer><a href="javascript:alert(1)">x</a><a href="mailto:a@b.c">mail</a></body></html>`
	p := Extract([]byte(page), "text/html; charset=utf-8", base)
	if p.Title != "Fares & Routes" {
		t.Errorf("title = %q", p.Title)
	}
	for _, want := range []string{"# Lisbon to New York", "Fares from 420\u00a0EUR, see the table.", "- Direct", "- One stop", "Café é A <b>"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("text has no %q:\n%s", want, p.Text)
		}
	}
	for _, not := range []string{"IGNORE ALL RULES", "color:red", "not text", "enable js", "svg text", "© Example", "Home"} {
		if strings.Contains(p.Text, not) {
			t.Errorf("text has %q:\n%s", not, p.Text)
		}
	}
	if len(p.Links) != 1 || p.Links[0].URL != "https://example.test/fares?x=1&y=2" || p.Links[0].Text != "the table" {
		t.Errorf("links = %+v", p.Links)
	}
}

// Without main or article the whole body is read; plain text is kept as it is; text is bounded.
func TestExtractBodyPlainAndBounds(t *testing.T) {
	base, _ := url.Parse("https://example.test/")
	p := Extract([]byte(`<html><body><div>One</div><div>Two<br>Three</div><a href="https://other.test/x">Other</a></body></html>`), "text/html", base)
	if !strings.Contains(p.Text, "One\nTwo\nThree") || len(p.Links) != 1 {
		t.Errorf("body: %q %+v", p.Text, p.Links)
	}
	p = Extract([]byte("line one\n<b>not a tag</b>\n"), "text/plain", base)
	if p.Text != "line one\n<b>not a tag</b>" || p.Title != "" {
		t.Errorf("plain: %q", p.Text)
	}
	big := "<p>" + strings.Repeat("word ", 20000) + "</p>"
	p = Extract([]byte(big), "text/html", base)
	if len(p.Text) > MaxText || !p.Truncated {
		t.Errorf("bounded: %d %v", len(p.Text), p.Truncated)
	}
	many := strings.Repeat(`<a href="/x">x</a>`, 500)
	if p = Extract([]byte(many), "text/html", base); len(p.Links) > MaxLinks {
		t.Errorf("links: %d", len(p.Links))
	}
	// invalid UTF-8 never reaches the model as such; Latin-1 is read as Latin-1
	if p = Extract([]byte("caf\xe9"), "text/plain; charset=iso-8859-1", base); p.Text != "café" {
		t.Errorf("latin-1: %q", p.Text)
	}
	if p = Extract([]byte("bad \xff byte"), "text/plain", base); !strings.Contains(p.Text, "bad") || strings.Contains(p.Text, "\xff") {
		t.Errorf("invalid utf-8: %q", p.Text)
	}
}

// Unclosed and odd markup never panics nor loops.
func TestExtractTolerant(t *testing.T) {
	base, _ := url.Parse("https://example.test/")
	for _, s := range []string{"<", "<<<>>>", "<script>never closed", "<a href='x'", "<!--", "<p attr=\"unterminated>text", "<title>t", "</p></div>", "<main><article>x</main>"} {
		_ = Extract([]byte(s), "text/html", base)
	}
}
