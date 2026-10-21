// Command needletail is the needletail engine's in-container runtime (docs/needletail.md):
// `needletail serve` runs an agent, `needletail oneshot` runs one turn for the bench and
// `needletail healthcheck` is the image's health check.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // TZ names resolve in an image without a zoneinfo database (schedules' cron)

	"github.com/camfinc/stormo/internal/needletail/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
