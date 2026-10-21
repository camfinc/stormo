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
		"```go\nx := a**b**c < 1\n```":                             "```\nx := a**b**c < 1\n```",
		"run `rm *.tmp` first":                                     "run `rm *.tmp` first",
		"| Name | Qty |\n|---|---:|\n| apple | 3 |\n| kiwi | 12 |": "```\nName   Qty\n-----  ---\napple  3\nkiwi   12\n```",
		"2 * 3 * 4":                                                "2 * 3 * 4",
	} {
		if got := Mrkdwn(in); got != want {
			t.Errorf("%q:\n got %q\nwant %q", in, got, want)
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
