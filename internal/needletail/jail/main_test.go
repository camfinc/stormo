package jail

import (
	"os"
	"testing"
)

// launchEnv makes the test binary the launcher (TestJailAsRoot starts it as `needletail jail`).
const launchEnv = "NT_JAIL_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(launchEnv) == "1" {
		os.Exit(Main(os.Args[1:], os.Stderr))
	}
	os.Exit(m.Run())
}
