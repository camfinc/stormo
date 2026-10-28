package messaging

import (
	"context"
	"fmt"
	"testing"
)

func TestVerifiedAmbientIsTrusted(t *testing.T) {
	for _, in := range []Input{{Verified: true, Ambient: true}, {Verified: true}} {
		if in.Untrusted() || UntrustedWhy(WithInput(context.Background(), in)) != "" {
			t.Fatalf("verified input quarantined: %+v", in)
		}
	}
	for _, in := range []Input{{Ambient: true}, {Verified: true, ContextUnverified: true}} {
		if !in.Untrusted() || UntrustedWhy(WithInput(context.Background(), in)) == "" {
			t.Fatalf("unverified input trusted: %+v", in)
		}
	}
}

func TestMutableQuarantineSharesMarkAndFailsClosed(t *testing.T) {
	ctx := WithQuarantine(context.Background(), nil)
	child := WithQuarantine(ctx, func(string) error { return fmt.Errorf("disk failed") })
	if err := MarkUntrusted(child, "marked KV"); err == nil {
		t.Fatal("persistence failure hidden")
	}
	if UntrustedWhy(ctx) != "marked KV" {
		t.Fatal("mark not shared")
	}
	calls := 0
	bound := WithQuarantine(ctx, func(string) error { calls++; return nil })
	if err := MarkUntrusted(bound, "another reason"); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	if UntrustedWhy(bound) != "marked KV" {
		t.Fatal("mark overwritten")
	}
	if err := MarkUntrusted(context.Background(), "marked"); err == nil {
		t.Fatal("missing provenance accepted")
	}
}
