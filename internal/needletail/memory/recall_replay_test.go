package memory

import (
	"strings"
	"testing"
	"time"
)

// Deterministic lexical evaluation: expected misses are measurements, not a claim that
// lexical retrieval understands translation. Held-out cases do not tune ranking constants.
func TestRecallHeldOutLexicalReplay(t *testing.T) {
	for _, tc := range []struct {
		name, text, query string
		want              bool
	}{
		{"english", "Acme ferry deposits require a twenty percent payment.", "What payment do Acme ferry deposits require?", true},
		{"spanish", "Los depósitos del ferry Acme requieren un pago del veinte por ciento.", "¿Qué pago requieren los depósitos del ferry Acme?", true},
		{"translation_miss", "Los depósitos del ferry requieren anticipo.", "How much is the boat booking downpayment?", false},
		{"irrelevant_overlap_known_false_positive", "The invoice portal exports invoices nightly.", "Which cabin does the invoice customer prefer aboard the cruise?", true},
		{"greeting", "Acme ferry deposits require payment.", "Gracias!", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(newHome(t, "", ""))
			if _, err := s.Save(SaveOptions{Text: tc.text}); err != nil {
				t.Fatal(err)
			}
			block, ids := s.Recall(Audience{}, tc.query, nil, 0, 0)
			if (len(ids) > 0) != tc.want {
				t.Fatalf("recall=%v block=%q want=%v", ids, block, tc.want)
			}
			t.Logf("fixture=%s language-independent lexical expected_hit=%v actual_hit=%v", tc.name, tc.want, len(ids) > 0)
		})
	}
}

func TestRecallReplayCurrentState(t *testing.T) {
	s := newStore(newHome(t, "", ""))
	audience := Audience{Scope: DMScope("example-user"), Conv: "root"}
	s.Where = func(string) Audience { return audience }
	r, err := s.Save(SaveOptions{Text: "Acme ferry deposits require twenty percent payment.", Audience: audience})
	if err != nil {
		t.Fatal(err)
	}
	rc := &Recaller{Store: s, Auto: true, Root: func(string) (string, bool) { return "root", true }}
	query := "What payment do Acme ferry deposits require?"
	for _, session := range []string{"root", "root", "compacted-child"} {
		if b := rc.Block(session, query, false); !strings.Contains(b, r.ID) {
			t.Fatalf("session=%s missing current record: %q", session, b)
		}
	}
	revised := "Acme ferry deposits require thirty percent payment."
	if _, err := s.Update(audience, r.ID, Change{Text: &revised}, false); err != nil {
		t.Fatal(err)
	}
	if b := rc.Block("root", query, false); !strings.Contains(b, "thirty") || strings.Contains(b, "twenty") {
		t.Fatal(b)
	}
	// A new process reconstructs recall from the same persisted records.
	restarted := &Recaller{Store: newStore(s.Home), Auto: true}
	restarted.Store.Where = s.Where
	if b := restarted.Block("root", query, false); !strings.Contains(b, "thirty") {
		t.Fatal(b)
	}
	audience = Audience{Scope: ChannelScope("example-channel"), Conv: "other"}
	if b := rc.Block("root", query, false); b != "" {
		t.Fatalf("audience leak: %s", b)
	}
	audience = Audience{Scope: DMScope("example-user"), Conv: "root"}
	expires := "2026-10-21"
	if _, err := s.Update(audience, r.ID, Change{Expires: &expires}, false); err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return t0.Add(48 * time.Hour) }
	if b := rc.Block("root", query, false); b != "" {
		t.Fatalf("expired: %s", b)
	}
	s.Now = func() time.Time { return t0 }
	if err := s.Delete(audience, r.ID, false); err != nil {
		t.Fatal(err)
	}
	if b := rc.Block("root", query, false); b != "" {
		t.Fatalf("deleted: %s", b)
	}
}

func TestRecallPacksSmallerRemainingCandidate(t *testing.T) {
	s := newStore(newHome(t, "", ""))
	// Strong overlap and importance make the oversized record rank first.
	large, err := s.Save(SaveOptions{Text: strings.Repeat("Acme ferry booking deposit payment ", 10), Importance: 5, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	small, err := s.Save(SaveOptions{Text: "Acme ferry booking deposit payment: cash.", Importance: 1, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	first, ids := s.Recall(Audience{}, "Acme ferry booking deposit payment", nil, 1, DefaultRecallBudget)
	if len(ids) != 1 || ids[0] != large.ID {
		t.Fatalf("fixture must rank oversized candidate first: %v %q", ids, first)
	}
	block, ids := s.Recall(Audience{}, "Acme ferry booking deposit payment", nil, 5, 140)
	if len(ids) != 1 || ids[0] != small.ID {
		t.Fatalf("smaller candidate not packed: %v %q", ids, block)
	}
}

// Replay the former root-conversation suppression policy as an explicit baseline,
// alongside disabled and candidate recall. All variants use identical stores and inputs.
func TestRecallPolicyBaselineReplay(t *testing.T) {
	for _, policy := range []string{"disabled", "conversation-suppression-baseline", "per-turn-candidate"} {
		t.Run(policy, func(t *testing.T) {
			s := newStore(newHome(t, "", ""))
			if _, err := s.Save(SaveOptions{Text: "Acme ferry deposits require twenty percent payment."}); err != nil {
				t.Fatal(err)
			}
			seen := map[string]bool{}
			hits, units := 0, 0
			for range 3 { // first question, follow-up, compacted descendant
				block := ""
				if policy != "disabled" {
					var ids []string
					var skip func(string) bool
					if policy == "conversation-suppression-baseline" {
						skip = func(id string) bool { return seen[id] }
					}
					block, ids = s.Recall(Audience{}, "What payment do Acme ferry deposits require?", skip, 0, 0)
					for _, id := range ids {
						seen[id] = true
					}
				}
				if block != "" {
					hits++
					units += Units(block)
				}
			}
			want := map[string]int{"disabled": 0, "conversation-suppression-baseline": 1, "per-turn-candidate": 3}[policy]
			if hits != want {
				t.Fatalf("hits=%d want=%d", hits, want)
			}
			t.Logf("policy=%s relevant_recall=%d/3 injected_utf16_units=%d model=deterministic-none prompt=recall-header-v1; tokens, task correctness, latency and stochastic uncertainty unmeasured", policy, hits, units)
		})
	}
}
