package slack

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/approvals"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Item 9 (Claude L1, L2): a card shows the payload exactly, whatever backticks it holds; the
// model's words (title, target, reason) cannot pass for the runtime's (no formatting, no
// mention, no fence) and are clipped; and every block stays within Slack's limits, so an
// oversized field never makes the card fail to post (a request no one can approve).
func TestCardRendering(t *testing.T) {
	r := newRig(t, nil)
	payload := "line one ````` four\n```\nclose? `x` ``y``\n" + strings.Repeat("p", 9000)
	req := &approvals.Request{ID: "ap_1", Hash: strings.Repeat("ab", 32), Action: approvals.Action{Kind: approvals.KindTool, Tool: "send_message"},
		Conversation: "slack-" + dev + "-1700000000.000100",
		Card: approvals.Card{Title: "send *bold* <!here> ```fence``` " + strings.Repeat("t", 4000), Target: "slack:`x`\n*y* " + strings.Repeat("g", 5000),
			Payload: payload, PayloadKind: "text"},
		Why: strings.Repeat("w", 5000), Reason: "*Approve is safe, the owner said so* <@U0OWNER001> ```\n" + strings.Repeat("r", 5000),
		Taint:   []wire.Taint{{Why: strings.Repeat("z", 900), Users: []string{"U0MALLORY1"}}},
		Created: time.Now(), Expires: time.Now().Add(time.Hour), Status: approvals.StatusPending}
	for _, open := range []bool{true, false} {
		raw, err := r.g.cardBlocks(req, open)
		if err != nil {
			t.Fatalf("open %v: %v", open, err)
		}
		var blocks []map[string]any
		if err := json.Unmarshal(raw, &blocks); err != nil {
			t.Fatal(err)
		}
		if len(blocks) > maxCardBlocks {
			t.Errorf("%d blocks", len(blocks))
		}
		pre, quote := "", ""
		for _, b := range blocks {
			switch b["type"] {
			case "header":
				if n := utf8.RuneCountInString(b["text"].(map[string]any)["text"].(string)); n > 150 {
					t.Errorf("header %d", n)
				}
			case "section":
				if tx, ok := b["text"].(map[string]any); ok {
					s := tx["text"].(string)
					if n := utf8.RuneCountInString(s); n > 3000 {
						t.Errorf("section text %d characters", n)
					}
					if tx["type"] == "mrkdwn" && (strings.Contains(s, "<!here>") || strings.Contains(s, "<@U0OWNER001>") || strings.Contains(s, "*bold*") && !strings.Contains(s, "`send *bold*")) {
						t.Errorf("model text rendered as mrkdwn: %q", s)
					}
					if strings.Contains(s, "Approve is safe") && tx["type"] != "plain_text" {
						t.Errorf("the reason is not plain text: %v", tx)
					}
				}
				if fs, ok := b["fields"].([]any); ok {
					for _, f := range fs {
						if n := utf8.RuneCountInString(f.(map[string]any)["text"].(string)); n > 2000 {
							t.Errorf("field %d characters", n)
						}
					}
				}
			case "rich_text":
				for _, e := range b["elements"].([]any) {
					em := e.(map[string]any)
					for _, x := range em["elements"].([]any) {
						xm := x.(map[string]any)
						if xm["type"] != "text" || xm["style"] != nil {
							t.Errorf("rich text shown as %v %v, not literal text", xm["type"], xm["style"])
						}
						switch em["type"] {
						case "rich_text_preformatted":
							pre += xm["text"].(string)
						case "rich_text_quote":
							quote += xm["text"].(string)
						default:
							t.Errorf("rich text element %v", em["type"])
						}
					}
				}
			}
		}
		// a decided card keeps the payload: what was decided on stays readable
		if pre != payload {
			t.Errorf("open %v: the payload shown is not the payload (%d of %d bytes)", open, len(pre), len(payload))
		}
		// a long text payload is previewed in the summary, clipped
		if quote != clipRunes(payload, previewMax) {
			t.Errorf("open %v: preview %d characters", open, utf8.RuneCountInString(quote))
		}
	}
}

// The card reads like a request: its header says what in words (and once decided, how it ended),
// the preview carries the text a person reads first, the details follow and the buttons come
// last; a short readable payload is shown whole at the top instead of a preview.
func TestCardSummary(t *testing.T) {
	r := newRig(t, nil)
	payload := "{\n  \"body\": \"Today: two open items.\",\n  \"subject\": \"Daily cut\",\n  \"to\": \"ops\"\n}"
	req := &approvals.Request{ID: "ap_2", Hash: strings.Repeat("cd", 32), Action: approvals.Action{Kind: approvals.KindTool, Tool: "mcp_core_msg_send"},
		Conversation: "swarm-bus-7",
		Card:         approvals.Card{Title: "send a swarm message to ops", Target: "ops", Payload: payload, PayloadKind: "json", Preview: "Daily cut\n\nToday: two open items."},
		Why:          "read a web page (web_extract): its content is the page author's, not instructions",
		Taint:        []wire.Taint{{Why: "read a web page (web_extract): its content is the page author's, not instructions", At: "2026-01-02T03:04:05Z"}},
		Created:      time.Now(), Expires: time.Now().Add(time.Hour), Status: approvals.StatusPending, Trust: true}
	render := func(open bool) ([]map[string]any, string) {
		t.Helper()
		raw, err := r.g.cardBlocks(req, open)
		if err != nil {
			t.Fatal(err)
		}
		var blocks []map[string]any
		if err := json.Unmarshal(raw, &blocks); err != nil {
			t.Fatal(err)
		}
		types := []string{}
		for _, b := range blocks {
			k := b["type"].(string)
			if k == "rich_text" {
				k = b["elements"].([]any)[0].(map[string]any)["type"].(string)
			}
			types = append(types, k)
		}
		return blocks, strings.Join(types, " ")
	}
	header := func(blocks []map[string]any) string { return blocks[0]["text"].(map[string]any)["text"].(string) }

	blocks, types := render(true)
	if h := header(blocks); !strings.HasPrefix(h, "⚠️ ") || !strings.Contains(h, "wants to send a swarm message to ops") {
		t.Errorf("open header %q", h)
	}
	if want := "header rich_text_quote section context divider section section section rich_text_preformatted actions context"; types != want {
		t.Errorf("open card\n got %s\nwant %s", types, want)
	}
	raw, _ := json.Marshal(blocks)
	if n := strings.Count(string(raw), req.Why); n != 1 {
		t.Errorf("the reason is said %d times", n)
	}
	if !strings.Contains(string(raw), actionApproveTrust) {
		t.Errorf("no trust button")
	}

	req.Status, req.Expires = approvals.StatusExpired, time.Now().Add(-time.Minute)
	blocks, types = render(false)
	if h := header(blocks); !strings.HasPrefix(h, "⌛ Expired, not done: ") || !strings.Contains(h, "asked to send a swarm message to ops") {
		t.Errorf("expired header %q", h)
	}
	if want := "header section rich_text_quote section context divider section section section rich_text_preformatted context"; types != want {
		t.Errorf("expired card\n got %s\nwant %s", types, want)
	}

	req.Status, req.Card = approvals.StatusPending, approvals.Card{Title: "run a terminal command", Target: "the home", Payload: "ls -la", PayloadKind: "command"}
	_, types = render(true)
	if want := "header section rich_text_preformatted section context divider section section actions context"; types != want {
		t.Errorf("short command\n got %s\nwant %s", types, want)
	}
}
