// Package stormo holds files the binary carries from the repository root: the instance standard
// (shipped inside the agent skill `stormo skill install` writes) and the example instance (the
// template `stormo new instance --template example` copies).
package stormo

import (
	"embed"
	"io/fs"
)

// InstancesDoc is docs/instances.md.
//
//go:embed docs/instances.md
var InstancesDoc string

// Only the instance's own parts: never generated or local files a working tree may hold there
// (dist/, .swarm/, secrets.local.yaml).
//
//go:embed examples/minimal/stormo.yaml examples/minimal/units examples/minimal/agents
//go:embed examples/minimal/bridge examples/minimal/personas
var example embed.FS

// Example is examples/minimal, rooted at the instance (stormo.yaml at the top).
func Example() fs.FS {
	sub, err := fs.Sub(example, "examples/minimal")
	if err != nil {
		panic(err)
	}
	return sub
}
