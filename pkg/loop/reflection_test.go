package loop

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
)

func TestReflectionExportBoundary(t *testing.T) {
	scrub := learning.NewScrubber(&instance.Instance{})
	encode := func(version int, text, audience, kind string) string {
		b, err := json.Marshal(map[string]any{"version": version, "proposals": []any{map[string]any{
			"id": "candidate-a", "text": text, "eventRefs": []string{"opaque-event"}, "audience": audience, "kind": kind,
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const procedure = "Check the quote currency before presenting a total."
	texts, err := reflectionTexts(encode(1, procedure, "agent", "procedure"), scrub)
	if err != nil || len(texts) != 1 || texts[0] != procedure {
		t.Fatalf("%v %v", texts, err)
	}
	for name, body := range map[string]string{
		"future version":     encode(2, procedure, "agent", "procedure"),
		"private audience":   encode(1, procedure, "dm:private", "procedure"),
		"user profile":       encode(1, procedure, "agent", "user"),
		"personal data":      encode(1, "Send the quote to client@example.com", "agent", "procedure"),
		"secret":             encode(1, "Token sk_abcdefghijklmnop", "agent", "procedure"),
		"oversized text":     encode(1, strings.Repeat("a", 2001), "agent", "procedure"),
		"oversized document": strings.Repeat("a", (512<<10)+1),
		"invalid JSON":       "{",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := reflectionTexts(body, scrub); err == nil {
				t.Fatal("unsafe export accepted")
			}
		})
	}
}
