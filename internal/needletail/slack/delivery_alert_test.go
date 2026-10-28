package slack

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/sched"
)

// The home notice about kept reports is the runtime's own: home only, no handler, no turn, no
// broadcast; ONE message with the count and the jobs' opaque ids, updated in place as the set
// changes, and updated to "recovered" when none waits.
func TestDeliveryAlertIsOneHomeNoticeUpdatedInPlace(t *testing.T) {
	h := &fakeHandlers{out: func(messaging.Outgoing) messaging.Verdict {
		return messaging.Verdict{Action: "rewrite", Text: "UNTRUSTED PAYLOAD <!channel>"}
	}}
	r := newRig(t, func(o *Options) { o.Handlers = h; o.Broadcast = true })
	ctx := messaging.WithUntrusted(context.Background(), "a payload that must not appear")
	ref, err := r.g.AlertDeliveries(ctx, sched.DeliveryAlert{JobIDs: []string{"a1b2c3"}})
	if err != nil {
		t.Fatal(err)
	}
	posts := r.fake.Posts()
	if len(posts) != 1 || posts[0].Channel != home || posts[0].Thread != "" || !strings.Contains(posts[0].Text, "1 scheduled report") || !strings.Contains(posts[0].Text, "a1b2c3") || ref.TS != posts[0].TS {
		t.Fatalf("unexpected alert: %+v %+v", posts, ref)
	}
	if strings.Contains(posts[0].Text, "<!") || strings.Contains(posts[0].Text, "UNTRUSTED") {
		t.Fatal(posts)
	}
	// a second report waiting: the same message, updated with the count
	if ref2, err := r.g.AlertDeliveries(ctx, sched.DeliveryAlert{JobIDs: []string{"a1b2c3", "d4e5f6"}, Ref: ref}); err != nil || ref2 != ref {
		t.Fatal(ref2, err)
	}
	// recovered
	if _, err := r.g.AlertDeliveries(ctx, sched.DeliveryAlert{Ref: ref}); err != nil {
		t.Fatal(err)
	}
	ups := r.fake.Updates()
	if len(r.fake.Posts()) != 1 || len(ups) != 2 || ups[0].TS != ref.TS || !strings.Contains(ups[0].Text, "2 scheduled reports") || !strings.Contains(ups[1].Text, "recovered") {
		t.Fatalf("posts %+v updates %+v", r.fake.Posts(), ups)
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
	if _, err := r.g.AlertDeliveries(context.Background(), sched.DeliveryAlert{JobIDs: []string{"a"}}); err == nil {
		t.Fatal("missing home accepted")
	}
	if len(r.fake.Posts()) != 0 {
		t.Fatal("missing home posted")
	}
	r2 := newRig(t, nil)
	r2.fake.SetFail("chat.postMessage", "channel_not_found")
	_, err := r2.g.AlertDeliveries(context.Background(), sched.DeliveryAlert{JobIDs: []string{"a"}})
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

func TestDeliveryErrorClasses(t *testing.T) {
	for code, want := range map[string]string{"channel_not_found": "not_in_channel", "not_in_channel": "not_in_channel", "is_archived": "not_in_channel",
		"missing_scope": "auth", "invalid_auth": "auth", "ratelimited": "transient", "internal_error": "transient"} {
		if got := (&APIError{Code: code}).DeliveryClass(); got != want {
			t.Errorf("%s: %s, want %s", code, got, want)
		}
	}
}
