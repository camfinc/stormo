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
		pre := ""
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
					if em["type"] != "rich_text_preformatted" {
						t.Errorf("rich text element %v", em["type"])
					}
					for _, x := range em["elements"].([]any) {
						pre += x.(map[string]any)["text"].(string)
					}
				}
			}
		}
		if open && pre != payload {
			t.Errorf("the payload shown is not the payload (%d of %d bytes)", len(pre), len(payload))
		}
		if !open && pre != "" {
			t.Errorf("a decided card shows the payload")
		}
	}
}
