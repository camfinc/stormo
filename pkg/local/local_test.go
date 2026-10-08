package local

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestPortsAreStable(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".swarm"), 0o755)
	os.WriteFile(filepath.Join(root, ".swarm", "ports.json"), []byte(`{"atlas": 18642, "scout": 18643, "nova": 18646}`), 0o644)
	for _, c := range []struct {
		id   string
		want int
	}{{"atlas", PortBase}, {"ann", PortBase + 2}, {"ben", PortBase + 3}, {"zed", PortBase + 5}, {"ann", PortBase + 2}} {
		if got, err := Port(root, c.id); err != nil || got != c.want {
			t.Errorf("%s: %d %v, want %d", c.id, got, err, c.want)
		}
	}
}

func TestComposeMountsTheInstanceAndTheLocalBaseline(t *testing.T) {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	p := Paths{Root: inst.Root, BaselineDir: "/x/.swarm/local/atlas/baseline", EnvFile: "/x/.swarm/env/atlas.env", StoreDir: "/x/.swarm/store", SharedDir: "/x/.swarm/shared"}
	spec := Render(a, hermes.New(inst), p, 18642, 120, "stormo-sidecar:test")
	if got := spec.Services["agent"].ExtraHosts; !slices.Equal(got, []string{"host.docker.internal:host-gateway"}) {
		t.Errorf("extra_hosts = %v", got)
	}
	for _, name := range []string{"rehydrate", "nap"} {
		s := spec.Services[name]
		if s.Image != "stormo-sidecar:test" || !slices.Contains(s.Volumes, p.BaselineDir+":/app/dist/atlas/baseline:ro") || !slices.Contains(s.Volumes, filepath.Join(inst.Root, "stormo.yaml")+":/app/stormo.yaml:ro") {
			t.Errorf("%s: %+v", name, s)
		}
		for _, v := range s.Volumes {
			if filepath.HasPrefix(v, filepath.Join(inst.Root, "dist")) {
				t.Errorf("%s mounts dist/: %s", name, v)
			}
		}
	}
	if got := spec.Services["nap"].Command; !slices.Equal(got, []string{"nap", "--loop"}) {
		t.Errorf("nap command = %v", got)
	}
}
