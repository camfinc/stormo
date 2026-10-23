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

// slackShaped are fake tokens in the shapes Slack issues (bot, user, app-level, workspace, refresh,
// rotating, browser session, legacy) and the old underscore spelling. Each is built from parts so
// no whole token sits in the source.
func slackShaped() []string {
	digits, fake := "123456789012-1234567890123", "FAKEfakeFAKEfakeFAKEfake"
	return []string{
		"xox" + "b-" + digits + "-" + fake,
		"xox" + "p-" + digits + "-123456789012-" + fake + "0000",
		"xox" + "a-2-" + digits + "-" + fake,
		"xox" + "r-" + digits + "-" + fake,
		"xox" + "e-1-" + digits + "-" + fake,
		"xox" + "e.xoxp-1-" + digits + "-" + fake,
		"xox" + "c-" + digits + "-" + fake,
		"xox" + "s-" + digits + "-" + fake,
		"xap" + "p-1-A0123456789-" + digits + "-" + fake,
		"xox" + "b_" + fake, // the old underscore form still matches
	}
}

// Slack tokens as Slack spells them (xoxb-…, xoxe.xoxp-…, xapp-…) are secrets to the scrub, whole:
// nothing of the token is left, and its digits are not taken for a phone or a card.
func TestScrubSlackTokens(t *testing.T) {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	s := NewScrubber(inst)
	for _, tok := range slackShaped() {
		text, findings := s.Scrub("the bot uses " + tok + " today")
		if text != "the bot uses [SECRET] today" || !slices.Equal(findings, []string{"secret"}) {
			t.Errorf("%s: %q %v", tok[:5], text, findings)
		}
	}
	for _, plain := range []string{"xoxo-from-the-team", "the xoxb- prefix names a bot token", "xapp-", "maxoxb-abcdefghijklmnop"} {
		if got, f := s.Scrub(plain); got != plain || len(f) != 0 {
			t.Errorf("false positive: %q %v", got, f)
		}
	}
}

// The Slack rule redacts only what no earlier rule took, so text that scrubbed one way before the
// rule existed scrubs the same way now: its ledger id holds, and so does a reviewer's rejection of
// it (rehydrate purges by the id of the scrubbed text). A Slack token after "Bearer " stays one
// [SECRET] with its header, as the Bearer rule always made it. Two shapes changed on purpose (no
// live ledger held either when the rule came): a bare Slack token, whose digits the phone or card
// rule took and whose letters stayed, is one [SECRET] now; and a Slack token used as an email's
// local part is [SECRET]@domain, no longer [EMAIL].
func TestScrubSlackKeepsEarlierIDs(t *testing.T) {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	after := NewScrubber(inst)
	before := &Scrubber{}
	for _, r := range after.rules {
		if r.re.String() != SlackTokenPattern {
			before.rules = append(before.rules, r)
		}
	}
	if len(before.rules) != len(after.rules)-1 {
		t.Fatalf("the Slack rule is not one of the scrubber's rules")
	}
	toks := slackShaped()
	hyphen, underscore := toks[:len(toks)-1], toks[len(toks)-1]
	fake := "FAKEfakeFAKEfakeFAKEfake0000"
	earlier := []string{
		"token sk_" + fake, "token acme_" + fake, "old " + underscore, "tg 123456789:AA" + fake + fake,
		"Bearer " + fake, "Authorization: Bearer abc.def-" + fake, "mail ann@example.com", "card 4111 1111 1111 1111",
		"Call +1 415 555 0123", "order AB12CD34", "xoxo-from-the-team", "the xoxb- prefix names a bot token",
	}
	same := append([]string{}, earlier...)
	for _, tok := range hyphen {
		same = append(same, "Bearer "+tok, "Authorization: Bearer "+tok+" works",
			"curl -H 'Authorization:  Bearer\t"+tok+"' https://slack.com/api/x")
		for _, e := range earlier {
			same = append(same, e+" with Bearer "+tok, "Bearer "+tok+" then "+e, e+"\nBearer "+tok+"\n"+e)
		}
	}
	for _, in := range same {
		b, bk := before.Scrub(in)
		a, ak := after.Scrub(in)
		if a != b || !slices.Equal(ak, bk) {
			t.Errorf("%q: scrubbed %q %v before the Slack rule, %q %v now", in, b, bk, a, ak)
		}
		for _, kind := range []Kind{KindMemory, KindUser} {
			if LearningID(kind, a) != LearningID(kind, b) {
				t.Errorf("%q: %s ledger id moved", in, kind)
			}
		}
	}
	// The shapes that changed, pinned.
	bot := hyphen[0]
	for in, want := range map[string]string{
		"the bot uses " + bot + " today":                 "the bot uses [SECRET] today",
		"the bot uses " + bot + ", call +1 415 555 0123": "the bot uses [SECRET], call [PHONE]",
		"write to " + bot + "@example.com":               "write to [SECRET]@example.com",
	} {
		b, _ := before.Scrub(in)
		if a, _ := after.Scrub(in); a != want || a == b {
			t.Errorf("%q: %q now (want %q), %q before", in, a, want, b)
		}
	}
}
