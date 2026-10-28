//go:build !linux

package jail

import (
	"errors"
	"fmt"
	"io"
)

// ABI is Linux only: elsewhere there is no Landlock (development).
func ABI() (int, error) { return 0, errors.New("Landlock is Linux only") }

// Restrict is Linux only.
func Restrict([]Rule, int, bool) error { return errors.New("Landlock is Linux only") }

// Main is Linux only: the runtime never wraps a command elsewhere.
func Main(_ []string, stderr io.Writer) int {
	fmt.Fprintln(stderr, "needletail jail: Landlock is Linux only")
	return ExitJail
}
