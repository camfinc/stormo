package needletail

import (
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

const minimal = "../../../examples/minimal"

// atlas is the example instance's atlas agent (on needletail).
func atlas(t *testing.T) (*instance.Instance, *manifest.Agent) {
	t.Helper()
	inst, err := instance.Load(minimal)
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return inst, a
}
