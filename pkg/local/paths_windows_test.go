package local

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestWindowsComposePaths(t *testing.T) {
	root := `C:\Users\Test User\Stormo`
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	p := Paths{Root: root, BaselineDir: filepath.Join(root, "baseline"), StoreDir: filepath.Join(root, "store"), EnvFile: filepath.Join(root, "agent.env"), SharedDir: filepath.Join(root, "workdir")}
	spec := Render(a, hermes.New(inst), p, 18642, 900, "stormo-sidecar:test")
	if len(spec.Services["agent"].ExtraHosts) != 0 {
		t.Fatal("Windows must use Docker Desktop host DNS")
	}
	for _, s := range spec.Services {
		for _, v := range append(s.Volumes, s.EnvFile...) {
			if strings.Contains(v, `\`) {
				t.Fatalf("non-portable compose path %q", v)
			}
		}
	}
	if got := spec.Services["rehydrate"].Volumes[0]; got != "C:/Users/Test User/Stormo/stormo.yaml:/app/stormo.yaml:ro" {
		t.Fatal(got)
	}
}
