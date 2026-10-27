package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

func TestDeliveryAlertIsFixedHomeOnlyWithoutHandlersOrTurn(t *testing.T) {
	h := &fakeHandlers{out: func(messaging.Outgoing) messaging.Verdict {
		return messaging.Verdict{Action: "rewrite", Text: "UNTRUSTED PAYLOAD <!channel>"}
	}}
	r := newRig(t, func(o *Options) { o.Handlers = h; o.Broadcast = true })
	ctx := messaging.WithUntrusted(context.Background(), "a payload that must not appear")
	if err := r.g.AlertDelivery(ctx); err != nil {
		t.Fatal(err)
	}
	posts := r.fake.Posts()
	if len(posts) != 1 || posts[0].Channel != home || posts[0].Thread != "" || posts[0].Text != deliveryFailureAlertText {
		t.Fatalf("unexpected alert: %+v", posts)
	}
	if strings.Contains(posts[0].Text, "<!") || strings.Contains(posts[0].Text, "UNTRUSTED") {
		t.Fatal(posts)
	}
	_, _, out := h.seen()
	if len(out) != 0 {
		t.Fatal("alert reached before_send", out)
	}
	if reserved, _ := r.turns.slots(); reserved != 0 {
		t.Fatal("alert invoked model turn", reserved)
	}
}

func TestDeliveryAlertFailsClosedWithoutHomeOrSlackAPI(t *testing.T) {
	r := newRig(t, func(o *Options) { o.HomeChannel = "" })
	if err := r.g.AlertDelivery(context.Background()); err == nil {
		t.Fatal("missing home accepted")
	}
	if len(r.fake.Posts()) != 0 {
		t.Fatal("missing home posted")
	}
	r2 := newRig(t, nil)
	r2.fake.SetFail("chat.postMessage", "channel_not_found")
	err := r2.g.AlertDelivery(context.Background())
	var api *APIError
	if !errors.As(err, &api) || api.Code != "channel_not_found" {
		t.Fatalf("API error lost: %v", err)
	}
	if len(r2.fake.Posts()) != 0 {
		t.Fatal("failed API posted")
	}
}

func TestDeliveryPermanentSlackErrors(t *testing.T) {
	for _, code := range []string{"channel_not_found", "not_in_channel", "is_archived", "missing_scope", "invalid_auth", "account_inactive"} {
		err := fmt.Errorf("delivery: %w", &APIError{Method: "chat.postMessage", Code: code})
		var classification interface{ DeliveryPermanent() bool }
		if !errors.As(err, &classification) || !classification.DeliveryPermanent() {
			t.Errorf("%s not permanent", code)
		}
	}
	for _, code := range []string{"ratelimited", "internal_error", "request_timeout", "unknown", ""} {
		if (&APIError{Code: code}).DeliveryPermanent() {
			t.Errorf("%s unexpectedly permanent", code)
		}
	}
	var absent *APIError
	if absent.DeliveryPermanent() {
		t.Fatal("nil permanent")
	}
}
