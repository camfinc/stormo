package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/camfinc/stormo/pkg/config"
	"github.com/camfinc/stormo/pkg/core"
	"github.com/camfinc/stormo/pkg/instance"
)

const capabilityEditUsage = `  capability show                        stormo.yaml's capabilities: each as written and as it runs, who is granted it, the file's hash
  capability set <name> --if-hash h      declare or change one by a JSON merge patch on stdin ({"template": "media"}), comments kept;
                                         the core loads it at its next restart
  capability remove <name> --if-hash h   take one out of stormo.yaml (refused while an agent is granted it)
                                         grants: config apply agents/<id>/agent.yaml with {"capabilities": [...]}`

// capabilityCmd edits stormo.yaml's capabilities (show, set, remove) and leaves the rest to the
// core's capability commands.
func capabilityCmd(inst *instance.Instance, sub string, rest []string) error {
	var f *config.CapabilitiesFile
	var err error
	switch sub {
	case "show":
		f, err = config.ShowCapabilities(inst)
	case "set":
		if len(rest) == 0 {
			return errors.New("usage: stormo capability set <name> --if-hash <h> < patch.json")
		}
		var patch []byte
		if patch, err = io.ReadAll(io.LimitReader(os.Stdin, 1<<20)); err != nil {
			return err
		}
		f, err = config.SetCapability(inst, rest[0], patch, *fIfHash)
	case "remove":
		if len(rest) == 0 {
			return errors.New("usage: stormo capability remove <name> --if-hash <h>")
		}
		f, err = config.RemoveCapability(inst, rest[0], *fIfHash)
	default:
		return core.CapabilityCommand(inst, sub, rest, output())
	}
	if err != nil {
		var ce *config.Error
		if errors.As(err, &ce) {
			return withCode(ce.Code, err)
		}
		return err
	}
	result(f, func(w io.Writer) {
		if len(f.Capabilities) == 0 {
			fmt.Fprintln(w, "no capabilities declared (stormo capability templates lists the bundled ones)")
		}
		for _, c := range f.Capabilities {
			how := c.Effective.Kind
			if c.Effective.Template != "" {
				how = "template " + c.Effective.Template
			}
			agents := "granted to no agent"
			if len(c.Agents) > 0 {
				agents = "granted to " + strings.Join(c.Agents, ", ")
			}
			fmt.Fprintf(w, "%-20s %-22s %s\n", c.Name, how, agents)
		}
		fmt.Fprintf(w, "hash %s\n", f.Hash)
	})
	return nil
}
