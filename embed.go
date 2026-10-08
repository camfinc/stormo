// Package stormo holds files the binary carries from the repository root (the instance standard,
// shipped inside the agent skill `stormo skill install` writes).
package stormo

import _ "embed"

// InstancesDoc is docs/instances.md.
//
//go:embed docs/instances.md
var InstancesDoc string
