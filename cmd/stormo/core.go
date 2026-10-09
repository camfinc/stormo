package main

import (
	"github.com/camfinc/stormo/pkg/core"
	"github.com/camfinc/stormo/pkg/instance"
)

func coreUsage() string { return core.Usage }

func runCore(inst *instance.Instance, sub string, rest []string) error {
	return core.Command(inst, sub, rest, core.CommandOptions{Out: output(), NoOpen: *fNoOpen, Force: *fForce, Connection: *fConnection})
}
