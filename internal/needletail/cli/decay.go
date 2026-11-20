package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/camfinc/stormo/internal/needletail/api"
	"github.com/camfinc/stormo/internal/needletail/approvals"
	"github.com/camfinc/stormo/internal/needletail/decay"
)

// Taint decay's timing (docs/needletail.md §7, "Taint decay"). The sweep is what decays a
// conversation: every decaySweep, each marked conversation no turn is running in, with time for
// the screen's worker to boot. A turn's own start only tries once, briefly: a person waiting on it
// never waits for a worker to boot (what it cannot decide, the sweep does).
const (
	decaySweep     = 2 * time.Minute
	decaySweepWait = 40 * time.Second
	decayTurnWait  = 8 * time.Second
	decayTurnLimit = 12 * time.Second
)

// startDecay is approvals.endorse: taint decay for rt's conversations, at each turn's start
// (rt.agent.Decay) and by sweepDecay. nil when the agent has none.
func startDecay(rt *runtime) *decay.Decay {
	a := rt.cfg.Approvals
	if a == nil || a.Endorse == nil {
		return nil
	}
	audit := filepath.Join(rt.cfg.StateDir(), "approvals-audit.jsonl")
	d := &decay.Decay{Store: rt.store, Cfg: *a.Endorse,
		Screen: func(ctx context.Context, capability string, input json.RawMessage, wait time.Duration) (json.RawMessage, error) {
			return screen(ctx, rt, capability, input, wait)
		},
		Audit: func(e map[string]any) { approvals.AppendAudit(audit, e, rt.logger.Printf) },
		Log:   func(s string) { rt.logger.Println(s) },
	}
	rt.agent.Decay = func(ctx context.Context, tip string) {
		if !d.Due(tip) {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, decayTurnLimit)
		defer cancel()
		if _, err := d.Run(ctx, tip, decayTurnWait); err != nil {
			rt.logger.Printf("decay: %s at the turn's start: %v (the sweep tries again)", tip, err)
		}
	}
	rt.logger.Printf("taint decay: after %s without new marked data, screened by %s", d.After(), a.Endorse.Screen)
	return d
}

// screen runs the screen capability as the agent's own job through the core (cap_run) and answers
// its output. The runtime asks it, not the model: the job's answer is probabilities, never text.
func screen(ctx context.Context, rt *runtime, capability string, input json.RawMessage, wait time.Duration) (json.RawMessage, error) {
	c := rt.core.Load()
	if c == nil {
		return nil, errors.New("the agent is not connected to a swarm core")
	}
	args, _ := json.Marshal(map[string]any{"capability": capability, "input": input, "wait_seconds": int(wait / time.Second)})
	res, err := c.Call(ctx, "cap_run", args)
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, fmt.Errorf("the core refused it: %s", clipText(res.Text, 300))
	}
	var job struct {
		Status string          `json:"status"`
		Output json.RawMessage `json:"output"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal([]byte(res.Text), &job); err != nil {
		return nil, fmt.Errorf("the core's answer could not be read: %v", err)
	}
	if job.Status != "done" {
		return nil, fmt.Errorf("job %s %s", job.Status, job.Error)
	}
	return job.Output, nil
}

// sweepDecay decays the marked conversations that are due, every decaySweep until ctx ends; one a
// turn runs in is left for the next sweep (or that turn's next start).
func sweepDecay(ctx context.Context, rt *runtime, srv *api.Server, d *decay.Decay) {
	t := time.NewTicker(decaySweep)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, tip := range rt.store.Marked() {
			if ctx.Err() != nil {
				return
			}
			if !d.Due(tip) {
				continue
			}
			release, err := srv.Hold(tip)
			if err != nil {
				continue // a turn runs there, or the runtime is closing
			}
			if _, err := d.Run(ctx, tip, decaySweepWait); err != nil {
				rt.logger.Printf("decay: %s: %v", tip, err)
			}
			release()
		}
	}
}
