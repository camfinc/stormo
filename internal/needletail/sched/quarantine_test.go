package sched

import (
	"context"
	"github.com/camfinc/stormo/internal/needletail/approvals"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"strings"
	"testing"
	"time"
)

func TestMarkedScheduledOutputCannotForward(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, jobJSON("job", `{"kind":"interval","minutes":60}`, `null`))
	ctx := messaging.WithQuarantine(context.Background(), nil)
	if err := messaging.MarkUntrusted(ctx, "marked KV"); err != nil {
		t.Fatal(err)
	}
	// a local job's report is its run record, tainted or not: nothing held, nothing on hold
	if errText, held := r.s.deliver(ctx, r.job("job"), outcome{status: wire.StatusOK, text: "marked data"}); errText != "" || held != "" {
		t.Fatal(errText, held)
	}
	// one that delivers somewhere is held for the owner's approval (none here: not delivered)
	j := r.job("job")
	j.Set("deliver", "slack:C0HOME0001")
	errText, held := r.s.deliver(ctx, j, outcome{status: wire.StatusOK, text: "marked data"})
	if errText != "" || !strings.Contains(held, "untrusted data") || strings.Contains(held, "marked data") {
		t.Fatal(errText, held)
	}
	var asked []string
	r.s.SetApprovals(func(_ context.Context, d approvals.Delivery, why string) (string, error) {
		asked = append(asked, d.Text+"|"+strings.Join(d.Targets, ",")+"|"+why)
		return "ap_test", nil
	})
	if _, held = r.s.deliver(ctx, j, outcome{status: wire.StatusOK, text: "marked data"}); !strings.Contains(held, "ap_test") || len(asked) != 1 || asked[0] != "marked data|slack:C0HOME0001|marked KV" {
		t.Fatal(held, asked)
	}
	if got := r.sink.taken(); len(got) != 0 {
		t.Fatal("marked output delivered", got)
	}
}
