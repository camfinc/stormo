package learning

import (
	"slices"
	"testing"
)

// A ledger entry flagged on any sighting stays flagged: a later clean sighting of the same text
// (or a flagged one after a clean one) unions the flags, so promotion keeps refusing it.
func TestUpsertUnionsPIIFlags(t *testing.T) {
	ledger := []*Learning{}
	s := Sighting{Kind: KindMemory, Text: "Use the token [SECRET] for the CRM.", Agent: "atlas", Instance: "i", Nap: "n1", At: "2026-10-01T00:00:00Z"}
	Upsert(&ledger, s)
	s.Nap, s.PII = "n2", []string{"secret"}
	e, created := Upsert(&ledger, s)
	if created || !slices.Equal(e.PII, []string{"secret"}) {
		t.Fatalf("flag of a later sighting lost: %+v", e)
	}
	s.Nap, s.PII = "n3", nil
	if e, _ = Upsert(&ledger, s); !slices.Equal(e.PII, []string{"secret"}) {
		t.Fatalf("a clean sighting cleared the flag: %+v", e)
	}
	s.Nap, s.PII = "n4", []string{"email", "secret"}
	if e, _ = Upsert(&ledger, s); !slices.Equal(e.PII, []string{"email", "secret"}) {
		t.Fatalf("flags not unioned in order: %+v", e)
	}
}
