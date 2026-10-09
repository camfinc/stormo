package learning

import (
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/instance"
)

// sect is a delimited memory format, as Hermes' (budgets count the delimiters).
const sect = engine.Delimited("\n§\n")

func TestParseAndPack(t *testing.T) {
	if got := ParseEntries(sect, "a\n§\n b \n§\n\n§\n"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("ParseEntries = %v", got)
	}
	kept, dropped := Pack(sect, []string{"aaaa", "bbbb", "cc"}, 9)
	if !slices.Equal(kept, []string{"aaaa", "cc"}) || !slices.Equal(dropped, []string{"bbbb"}) {
		t.Errorf("Pack = %v %v", kept, dropped)
	}
}

func TestBudgetsCountUTF16Units(t *testing.T) {
	// Budgets count UTF-16 code units: an emoji is two.
	if UTF16Len("🚨a") != 3 || UTF16Len("ñ") != 1 {
		t.Errorf("UTF16Len = %d, %d", UTF16Len("🚨a"), UTF16Len("ñ"))
	}
}

func TestMergeHot(t *testing.T) {
	entries, purged, deferred := MergeHot(sect, []string{"live one", "bad entry"}, []string{"seed one", "seed two that is long"},
		map[string]bool{Normalize("Bad   Entry"): true}, 40, 0.5)
	if !slices.Equal(purged, []string{"bad entry"}) || !slices.Equal(entries, []string{"live one", "seed one"}) || !slices.Equal(deferred, []string{"seed two that is long"}) {
		t.Errorf("entries=%v purged=%v deferred=%v", entries, purged, deferred)
	}
}

func TestNormalizeFoldsUnicodeWhitespace(t *testing.T) {
	if got := Normalize("  Hello  Mundo\n "); got != "hello mundo" {
		t.Errorf("Normalize = %q", got)
	}
}

func TestScrub(t *testing.T) {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	s := NewScrubber(inst)
	text, findings := s.Scrub("Call +1 415 555 0123, order AB12CD34, mail ann@example.com, token acme_testFAKEtoken0000")
	for _, leak := range []string{"555", "AB12CD34", "ann@", "testFAKE"} {
		if strings.Contains(text, leak) {
			t.Errorf("%q left in %q", leak, text)
		}
	}
	if !slices.Equal(findings, []string{"email", "order-id", "phone", "secret"}) {
		t.Errorf("findings = %v", findings)
	}
	plain := "Deals for the next 2 days, grouped by close date; bump after 2 hours."
	if got, f := s.Scrub(plain); got != plain || len(f) != 0 {
		t.Errorf("ordinary text changed: %q %v", got, f)
	}
	// require: eight capitals without a digit are a word, not an order id.
	if got, f := s.Scrub("URGENT: call ABSOLUTE"); got != "URGENT: call ABSOLUTE" || len(f) != 0 {
		t.Errorf("false positive: %q %v", got, f)
	}
}
