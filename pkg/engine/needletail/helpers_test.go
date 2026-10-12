package needletail

import (
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

const minimal = "../../../examples/minimal"

// atlas is the example instance's atlas agent switched to needletail.
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
	a.Engine.Kind, a.Engine.Version, a.Engine.ImageTag = "needletail", "0.1.0", "v0.1.0"
	return inst, a
}
