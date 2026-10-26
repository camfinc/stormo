package sched

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

func TestQuarantinedMonitorNeverBecomesTrustedBaseline(t *testing.T) {
	for _, action := range []string{"pass", "reply", "silent", "cached"} {
		t.Run(action, func(t *testing.T) {
			t0, _ := time.Parse(time.RFC3339, t0String)
			r := bareRig(t, t0, gatedJob("mon", "monitor_script", "monitor.luau"))
			writeScript(t, r.home, "monitor.luau", "synthetic callback")
			marked := true
			r.s.o.Luau = func(ctx context.Context, _, _ string) (bool, string) {
				if marked {
					_ = messaging.MarkUntrusted(ctx, "marked monitor value")
					return true, "UNVERIFIED INSTRUCTION"
				}
				return true, "clean value"
			}
			if action == "reply" || action == "silent" {
				r.s.o.OnSchedule = func(context.Context, string, string, map[string]any) messaging.Verdict {
					return messaging.Verdict{Action: action, Text: "handled"}
				}
			}
			r.turns.reply("mon", "observed")
			if action == "cached" {
				r.turns.busy = true
			}
			r.tick()
			if action == "cached" {
				r.turns.busy = false
				r.advance(30 * time.Second)
				r.tick()
			}
			snap := filepath.Join(r.s.state, "monitor", "mon.out")
			if b, err := os.ReadFile(snap); !os.IsNotExist(err) {
				t.Fatalf("marked monitor persisted for %s: %s %v", action, b, err)
			}
			if _, had := r.job("mon").monitor(); had {
				t.Fatal("marked baseline hash persisted")
			}
			// A later clean observation must be a first baseline, never a diff containing
			// the previous marked text. It can resume ordinary baseline persistence.
			marked = false
			r.s.o.OnSchedule = nil
			r.advance(time.Hour)
			r.tick()
			calls := r.turns.taken()
			if len(calls) == 0 || strings.Contains(calls[len(calls)-1].input, "UNVERIFIED INSTRUCTION") {
				t.Fatal("old marked output laundered", calls)
			}
			if b, err := os.ReadFile(snap); err != nil || string(b) != "clean value" {
				t.Fatalf("clean baseline did not persist: %q %v", b, err)
			}
		})
	}
}

func TestQuarantinedErrorsNeverAppearInPublicJobState(t *testing.T) {
	t0, _ := time.Parse(time.RFC3339, t0String)
	r := bareRig(t, t0, jobJSON("job", `{"kind":"interval","minutes":60}`, `null`))
	ctx := messaging.WithQuarantine(context.Background(), nil)
	_ = messaging.MarkUntrusted(ctx, "marked data")
	r.s.finish(ctx, r.job("job"), outcome{status: wire.StatusError, err: "UNVERIFIED ERROR INSTRUCTION", pause: "UNVERIFIED PAUSE INSTRUCTION", text: "UNVERIFIED OUTPUT"})
	j := r.job("job")
	listed, _ := json.Marshal(view(j))
	if strings.Contains(string(listed), "UNVERIFIED") {
		t.Fatal("public job metadata launders marked data", string(listed))
	}
	if j.Str("last_error") == "" || j.Str("paused_reason") == "" {
		t.Fatal("missing safe diagnostic state")
	}
	if strings.Contains(j.Str("last_delivery_error"), "UNVERIFIED") {
		t.Fatal("delivery metadata launders data")
	}
}
