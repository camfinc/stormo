package main

import (
	"os"

	"github.com/camfinc/stormo/pkg/core"
	"github.com/camfinc/stormo/pkg/instance"
)

func coreUsage() string {
	return core.Usage + "\n" + runnerUsage + "\n" + core.CapabilityUsage + "\n" + capabilityEditUsage + "\n" + core.DeviceUsage
}

func runCore(inst *instance.Instance, sub string, rest []string) error {
	return core.Command(inst, sub, rest, core.CommandOptions{Out: output(), NoOpen: *fNoOpen, Force: *fForce, Connection: *fConnection})
}

func deviceCmd(inst *instance.Instance, sub string, rest []string) error {
	st, _ := os.Stdin.Stat()
	return core.DeviceCommand(inst, sub, rest, output(), core.DeviceOptions{Name: *fName, Grant: *fGrant, Agents: *fAgents, Yes: *fYes,
		Ask: st != nil && st.Mode()&os.ModeCharDevice != 0})
}
