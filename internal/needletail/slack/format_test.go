package slack

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The model's Markdown as Slack shows it (Hermes' format_message rules).
func TestMrkdwn(t *testing.T) {
	for in, want := range map[string]string{
		"**bold** and *italic* and ***both***":                     "*bold* and _italic_ and *_both_*",
		"## Plan\n- one\n- two":                                    "*Plan*\n- one\n- two",
		"See [the doc](https://example.com/a?b=1&c=2).":            "See <https://example.com/a?b=1&c=2|the doc>.",
		"a < b && c > d":                                           "a &lt; b &amp;&amp; c &gt; d",
		"ping <@U0ALLOWED1> in <#C0DEVCHAN1>":                      "ping <@U0ALLOWED1> in <#C0DEVCHAN1>",
		"tell <!channel> now":                                      "tell &lt;!channel&gt; now",
		"~~old~~ new":                                              "~old~ new",
		"> quoted **line**":                                        "> quoted *line*",
		"```go\nx := a**b**c < 1\n```":                             "```\nx := a**b**c &lt; 1\n```",
		"run `rm *.tmp` first":                                     "run `rm *.tmp` first",
		"| Name | Qty |\n|---|---:|\n| apple | 3 |\n| kiwi | 12 |": "```\nName   Qty\n-----  ---\napple  3\nkiwi   12\n```",
		"2 * 3 * 4":                                                "2 * 3 * 4",
		// Slack wants &, < and > escaped in all text, code included
		"`a<b && c>d`":              "`a&lt;b &amp;&amp; c&gt;d`",
		"```\n<!channel> & co\n```": "```\n&lt;!channel&gt; &amp; co\n```",
		// links: a URL's <, > and | are percent-encoded, the label escaped; other schemes stay text
		"[a <b> & c](https://x.example/p?q=<!here>|y)": "<https://x.example/p?q=%3C!here%3E%7Cy|a &lt;b&gt; &amp; c>",
		"[click](javascript:alert(1))":                 "[click](javascript:alert(1))",
		// every special mention is escaped: broadcasts, user groups and dates
		"hey <!here|here> and <!everyone>":                         "hey &lt;!here|here&gt; and &lt;!everyone&gt;",
		"team <!subteam^S0TEAM0001|@ops> look":                     "team &lt;!subteam^S0TEAM0001|@ops&gt; look",
		"due <!date^1700000000^{date}|today>":                      "due &lt;!date^1700000000^{date}|today&gt;",
		"<https://x.example|<!channel>> and <@U0ALLOWED1|<!here>>": "&lt;https://x.example|&lt;!channel&gt;&gt; and &lt;@U0ALLOWED1|&lt;!here&gt;&gt;",
	} {
		if got := Mrkdwn(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
	}
}

// A broadcast is kept only when the caller asks for one (send_message's broadcast, granted).
func TestMrkdwnBroadcast(t *testing.T) {
	for in, want := range map[string]string{
		"<!channel> standup in 5":         "<!channel> standup in 5",
		"@here the build is red":          "<!here> the build is red",
		"ping <!subteam^S0TEAM0001|@ops>": "ping <!subteam^S0TEAM0001|@ops>",
		"due <!date^1700000000^{date}|x>": "due &lt;!date^1700000000^{date}|x&gt;",
		"mail me@here.example":            "mail me@here.example",
	} {
		if got := Format(in, true); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
		}
		if got := Format(in, false); strings.Contains(got, "<!") {
			t.Errorf("%q without broadcast: %q", in, got)
		}
	}
}

// Long replies are split at a line break, numbered, and a code block cut in two is closed and
// opened again.
func TestSplit(t *testing.T) {
	if p := Split("short", 100); len(p) != 1 || p[0] != "short" {
		t.Errorf("short: %q", p)
	}
	para := strings.Repeat("word ", 30) + "\n"
	text := strings.Repeat(para, 10) + "```\n" + strings.Repeat("code line\n", 40) + "```\nend"
	parts := Split(text, 400)
	if len(parts) < 3 {
		t.Fatalf("%d parts", len(parts))
	}
	for i, p := range parts {
		if utf8.RuneCountInString(p) > 400 {
			t.Errorf("part %d is %d long", i, utf8.RuneCountInString(p))
		}
		if !strings.HasSuffix(p, " ("+strconv.Itoa(i+1)+"/"+strconv.Itoa(len(parts))+")") {
			t.Errorf("part %d: %q", i, p[max(0, len(p)-20):])
		}
		if strings.Count(p, "```")%2 != 0 {
			t.Errorf("part %d leaves a code block open", i)
		}
	}
	joined := strings.Join(parts, "")
	if strings.Count(joined, "code line") != 40 || strings.Count(joined, "word") != 300 || !strings.Contains(joined, "end") {
		t.Error("text lost in the split")
	}
}

// A cut never falls inside inline code: it moves before the span's opening backtick.
func TestSplitInlineCode(t *testing.T) {
	text := strings.Repeat("x", 50) + " `a b c d e f g h i j k l m n o p q r s` after"
	parts := Split(text, 90)
	if len(parts) < 2 {
		t.Fatalf("%q", parts)
	}
	for _, p := range parts {
		body := p[:strings.LastIndex(p, " (")]
		if strings.Count(body, "`")%2 != 0 {
			t.Errorf("a part cuts the inline code: %q", parts)
		}
	}
}

// A fenced block cut in two is closed right after its last line, with no blank line, whatever
// the characters.
func TestSplitFenceNoBlankLine(t *testing.T) {
	for _, line := range []string{"ñandú éé\n", "nandu ee\n"} {
		parts := Split("intro\n```\n"+strings.Repeat(line, 30)+"```\nend", 100)
		for i, p := range parts[:len(parts)-1] {
			if strings.Contains(p, "\n\n```") {
				t.Errorf("part %d has a blank line before its closing fence: %q", i, p)
			}
		}
		if j := strings.Join(parts, ""); strings.Count(j, strings.TrimSpace(line)) != 30 {
			t.Errorf("lines lost: %q", parts)
		}
	}
}
