package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/link"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/ops"
	"github.com/camfinc/stormo/pkg/place"
	"github.com/camfinc/stormo/pkg/secrets"
	"github.com/camfinc/stormo/pkg/shared"
	"github.com/camfinc/stormo/pkg/version"
)

const runnerUsage = `  runner <placement>                     run this host's agents for a runner placement (stormo.yaml placements:)
                                         and take start/stop/restart orders from the core: SWARM_CORE_URL
                                         and STORMO_RUNNER_TOKEN (an operate token) in the environment`

// runRunner serves a runner placement from this host: it dials the core and carries out its
// lifecycle orders with this checkout of the instance, as `stormo start|stop|restart` would here.
func runRunner(inst *instance.Instance, placement string) error {
	p, ok := inst.PlacementNamed(placement)
	if !ok || p.Kind != instance.PlacementRunner {
		return fmt.Errorf("%q is not a runner placement of this instance (stormo.yaml placements: {%s: {kind: runner}})", placement, placement)
	}
	coreURL, token := os.Getenv("SWARM_CORE_URL"), os.Getenv("STORMO_RUNNER_TOKEN")
	if coreURL == "" || token == "" {
		return errors.New("set SWARM_CORE_URL (where this host reaches the core) and STORMO_RUNNER_TOKEN (stormo core token add <name> operate)")
	}
	if inst.CoreURLFor(placement) == "" {
		return fmt.Errorf("stormo.yaml core.urls.%s (or core.url) must say where this placement's agents reach the core", placement)
	}
	// Agents started here are built and wired for this placement (local.Placement).
	os.Setenv("SWARM_PLACEMENT", placement)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// This host's copy of the shared space follows the core's (the layers its agents use).
	syncer := &link.Syncer{CoreURL: coreURL, Token: token, Placement: placement, Dir: shared.LocalDir(inst.Root),
		Layers:    func() []string { return append([]string{"group"}, manifest.UnitIDs(inst.Root)...) },
		StateFile: filepath.Join(inst.Root, ".swarm", "runner", "sync-"+placement+".json"), Logf: log.Printf}
	go syncer.Run(ctx, 15*time.Second)
	link.RunRunner(ctx, link.RunnerOptions{
		CoreURL: coreURL, Token: token, Placement: placement, Version: version.String(),
		Act: func(ctx context.Context, o link.Order, log func(string)) (link.OrderResult, error) {
			if o.Action == "capability" && o.Job != nil {
				return runJob(ctx, inst, o)
			}
			action, agent := o.Action, o.Agent
			d := ops.DefaultDeps(inst, true)
			d.Log = log
			var err error
			switch action {
			case "start":
				err = ops.Start(place.Local, agent, d)
			case "stop":
				err = ops.Stop(place.Local, agent, d)
			case "restart":
				err = ops.Restart(place.Local, agent, d)
			case "status":
			default:
				return link.OrderResult{}, fmt.Errorf("unknown order %q", action)
			}
			st, serr := ops.AgentStatus(place.Local, d, []string{agent})
			state := ""
			if serr == nil && len(st) == 1 {
				state = st[0].State
			}
			return link.OrderResult{State: state}, err
		},
	})
	return nil
}

// runJob runs a capability's container here for the core, with the secrets it names from this
// host's secrets file (shared layer).
func runJob(ctx context.Context, inst *instance.Instance, o link.Order) (link.OrderResult, error) {
	j := o.Job
	env := map[string]string{}
	if len(j.Secrets) > 0 {
		f, err := secrets.Load(secrets.Path(inst.Root))
		if err != nil {
			return link.OrderResult{}, err
		}
		for _, n := range j.Secrets {
			if v, ok := secrets.SharedValue(f, n); ok {
				env[n] = v
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(j.TimeoutMinutes)*time.Minute)
	defer cancel()
	out, err := link.RunContainer(ctx, j.Image, j.Command, env, j.Input)
	if err != nil {
		return link.OrderResult{}, err
	}
	return link.OrderResult{Output: out}, nil
}
