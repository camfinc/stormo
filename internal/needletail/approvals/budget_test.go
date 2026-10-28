package approvals

import (
	"context"
	"errors"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Item 16 (Claude M2): the daily caps are separate budgets, so untrusted input cannot spend the
// owner's: each taint author's (or, without one, each tainted conversation's) requests, the
// trusted conversations' risky commands, and held reports; a trusted request denied, expired
// or failed does not count against trusted work.
func TestDailyBudgetsAreSeparate(t *testing.T) {
	w := newWorld(t)
	w.svc.o.PerDay, w.svc.o.PerConversation = 3, 100
	ctx := context.Background()
	byAuthor := func(conv, author string) Ask {
		a := ask(conv)
		a.Taint = []wire.Taint{{Why: "unverified", Users: []string{author}}}
		return a
	}
	trusted := func(conv string) Ask {
		a := ask(conv)
		a.Taint, a.Trust = nil, false
		return a
	}
	for i := range 3 {
		if _, err := w.svc.Request(ctx, byAuthor("conv-m"+string(rune('a'+i)), "U0MALLORY1")); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := w.svc.Request(ctx, byAuthor("conv-mz", "U0MALLORY1")); !errors.Is(err, ErrCap) {
		t.Fatalf("one author past the cap: %v", err)
	}
	if _, err := w.svc.Request(ctx, byAuthor("conv-e", "U0EVE00001")); err != nil {
		t.Fatalf("another author's budget was spent: %v", err)
	}
	// trusted work is untouched by the tainted budget, and a denied request gives its slot back
	for i := range 5 {
		r, err := w.svc.Request(ctx, trusted("dm-owner"))
		if err != nil {
			t.Fatalf("trusted request %d refused: %v", i, err)
		}
		if _, err := w.svc.Decide(ctx, Decision{ID: r.ID, Hash: r.Hash, By: "U0OWNER001"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		if _, err := w.svc.Request(ctx, trusted("dm-owner")); err != nil {
			t.Fatalf("trusted request %d refused: %v", i, err)
		}
	}
	if _, err := w.svc.Request(ctx, trusted("dm-owner")); !errors.Is(err, ErrCap) {
		t.Fatalf("trusted past its own cap: %v", err)
	}
	// held reports have their own budget
	if _, err := w.svc.Request(ctx, Ask{Action: Action{Kind: KindDelivery, Delivery: &Delivery{JobID: "j1", Text: "r", Targets: []string{"slack"}}},
		Card: Card{Title: "deliver", Payload: "r", PayloadKind: "text"}, Why: "tainted"}); err != nil {
		t.Fatalf("a held report refused: %v", err)
	}
}
