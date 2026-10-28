package cli

import (
	"os"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/privsep"
)

// The tests boot runtimes in the test process: run as root (a CI container), the uid split would
// drop the test process itself to the runtime's user. privsep's own tests cover the split.
func TestMain(m *testing.M) {
	if os.Geteuid() == 0 {
		os.Setenv(privsep.OffEnv, "off")
	}
	os.Exit(m.Run())
}
