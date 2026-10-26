package sched

import (
	"context"
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
	if err := r.s.deliver(ctx, r.job("job"), outcome{status: wire.StatusOK, text: "marked data"}); !strings.Contains(err, "scheduled forwarding") {
		t.Fatal(err)
	}
	if got := r.sink.taken(); len(got) != 0 {
		t.Fatal("marked output delivered", got)
	}
}
