package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/camfinc/stormo/internal/needletail/config"
	"github.com/camfinc/stormo/internal/needletail/jail"
)

// JailEnv turns the jail off for a runtime that splits (development only: the agent's commands
// then reach the whole container). Never set in the image.
const JailEnv = "NEEDLETAIL_JAIL"

// jailState is the jail the runtime gives the agent's commands: its launcher (nil: none) and how
// it is reported (the activity's jail, the log): "landlock ABI <n>", or "off: <why>".
type jailState struct {
	launcher *jail.Launcher
	state    string
}

// jailFor decides the jail at boot: on whenever the uid split is (the image, started as root on
// Linux) and the kernel has Landlock, through self (the runtime's binary). A kernel without
// Landlock (Fargate today, docs/needletail.md §7) leaves it off, said loudly once: the agent
// keeps working, with the reach the uid split and the container give its commands.
func jailFor(split bool, spec jail.Spec, abi func() (int, error), self func() (string, error), getenv func(string) string, logf func(string)) jailState {
	if !split {
		return jailState{state: "off: the uid split is off"}
	}
	if getenv(JailEnv) == "off" {
		logf("jail off (" + JailEnv + "=off): the agent's commands reach the whole container, development only")
		return jailState{state: "off: " + JailEnv + "=off"}
	}
	n, err := abi()
	if err != nil {
		logf(fmt.Sprintf("JAIL OFF: %v. The agent's commands (terminal, scheduled scripts, monitors) can read the whole "+
			"container's filesystem and write wherever their user may (/tmp too, unless the root filesystem is read-only); only the uid split keeps them out "+
			"of the runtime's files. Run on a kernel with Landlock (5.13+, enabled) to confine them to the home and the shared layers.", err))
		return jailState{state: "off: " + err.Error()}
	}
	path, err := self()
	if err != nil {
		logf(fmt.Sprintf("JAIL OFF: the runtime's own binary is not found (%v): the agent's commands are not confined", err))
		return jailState{state: "off: the runtime's binary is not found"}
	}
	logf(fmt.Sprintf("jail: the agent's commands run confined by Landlock (ABI %d) to %s and %v, with read and execute on the system's programs", n, spec.Home, spec.Shared))
	return jailState{launcher: &jail.Launcher{Self: path, Spec: spec}, state: fmt.Sprintf("landlock ABI %d", n)}
}

// jailSpec is the agent's jail: its home and its shared layers, absolute.
func jailSpec(cfg *config.Config) jail.Spec {
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	s := jail.Spec{Home: abs(cfg.Home)}
	for _, d := range cfg.SharedRoots() {
		s.Shared = append(s.Shared, abs(d))
	}
	return s
}

// executable is the runtime's binary, read before the uid split (the launcher of the jail).
func executable() func() (string, error) {
	p, err := os.Executable()
	return func() (string, error) { return p, err }
}
