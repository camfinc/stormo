package loop

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
)

func TestReflectionExportBoundary(t *testing.T) {
	scrub := learning.NewScrubber(&instance.Instance{})
	proposal := func(text, audience, kind string) map[string]any {
		return map[string]any{"id": "candidate-a", "text": text, "eventRefs": []string{"opaque-event"}, "audience": audience, "kind": kind}
	}
	encode := func(version int, proposals ...map[string]any) string {
		b, err := json.Marshal(map[string]any{"version": version, "proposals": proposals})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	const procedure = "Check the quote currency before presenting a total."
	got, notices := reflectionTexts(encode(1, proposal(procedure, "agent", "procedure")), scrub)
	if len(got) != 1 || got[0].Text != procedure || len(got[0].PII) != 0 || len(notices) != 0 {
		t.Fatalf("%v %v", got, notices)
	}

	// A whole export this build cannot read is skipped with one notice; nothing from it is kept.
	for name, body := range map[string]string{
		"future version": encode(2, proposal(procedure, "agent", "procedure")),
		"too many proposals": encode(1, func() []map[string]any {
			l := []map[string]any{}
			for range 101 {
				l = append(l, proposal(procedure, "agent", "procedure"))
			}
			return l
		}()...),
		"oversized document": strings.Repeat("a", (512<<10)+1),
		"invalid JSON":       "{",
	} {
		t.Run(name, func(t *testing.T) {
			if got, notices := reflectionTexts(body, scrub); len(got) != 0 || len(notices) != 1 {
				t.Fatalf("kept %v, notices %v", got, notices)
			}
		})
	}

	// One bad proposal is skipped with a notice; the others still come through. A flagged one is
	// kept scrubbed and flagged (held for a person, never promoted), like any flagged memory.
	got, notices = reflectionTexts(encode(1,
		proposal(procedure, "dm:private", "procedure"),
		proposal(procedure, "agent", "user"),
		proposal(strings.Repeat("a", 2001), "agent", "procedure"),
		proposal("Send the quote to client@example.com", "agent", "procedure"),
		proposal("Token sk_abcdefghijklmnop", "agent", "procedure"),
		proposal(procedure, "agent", "procedure"),
	), scrub)
	if len(notices) != 5 {
		t.Errorf("notices %v", notices)
	}
	kept := map[string][]string{}
	for _, g := range got {
		kept[g.Text] = g.PII
	}
	if len(got) != 3 || kept[procedure] == nil && len(kept) != 3 {
		t.Fatalf("kept %v", got)
	}
	for text, pii := range kept {
		if text == procedure {
			continue
		}
		if len(pii) == 0 || strings.Contains(text, "client@example.com") || strings.Contains(text, "sk_abcdefghijklmnop") {
			t.Errorf("flagged proposal kept as %q %v", text, pii)
		}
	}
	for _, n := range notices {
		if strings.Contains(n, "example.com") || strings.Contains(n, "sk_") || strings.Contains(n, "dm:private") {
			t.Errorf("a notice carries proposal text: %q", n)
		}
	}
}

// Two proposals that scrub to one text keep every flag either had: a flagged sighting is never
// hidden behind an unflagged twin that came first.
func TestReflectionDedupKeepsFlags(t *testing.T) {
	scrub := learning.NewScrubber(&instance.Instance{})
	body, _ := json.Marshal(map[string]any{"version": 1, "proposals": []any{
		map[string]any{"id": "a", "text": "Put the token [SECRET] in the header.", "eventRefs": []string{}, "audience": "agent", "kind": "procedure"},
		map[string]any{"id": "b", "text": "Put the token sk_abcdefghijklmnop in the header.", "eventRefs": []string{}, "audience": "agent", "kind": "procedure"},
	}})
	got, _ := reflectionTexts(string(body), scrub)
	if len(got) != 1 || !slices.Contains(got[0].PII, "secret") {
		t.Fatalf("the flagged twin lost its flag: %+v", got)
	}
}
