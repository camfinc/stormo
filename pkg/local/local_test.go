package local

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
)

func TestPortsAreStable(t *testing.T) {
	t.Setenv("STORMO_HOME", t.TempDir())
	defer func(f func(int) bool) { portFree = f }(portFree)
	portFree = func(int) bool { return true }
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
	p := Paths{Root: inst.Root, BaselineDir: "/x/.swarm/local/atlas/baseline", EnvFile: "/x/agents/atlas/data/agent.env", StoreDir: "/x/agents/atlas/data/store", SharedDir: "/x/.swarm/shared"}
	spec := Render(a, hermes.New(inst), p, 18642, 120, "stormo-sidecar:test")
	if got := spec.Services["agent"].ExtraHosts; !slices.Equal(got, []string{"host.docker.internal:host-gateway"}) {
		t.Errorf("extra_hosts = %v", got)
	}
	for _, name := range []string{"rehydrate", "nap"} {
		s := spec.Services[name]
		if s.Image != "stormo-sidecar:test" || !slices.Contains(s.Volumes, p.BaselineDir+":/app/dist/atlas/baseline:ro") || !slices.Contains(s.Volumes, filepath.Join(inst.Root, "stormo.yaml")+":/app/stormo.yaml:ro") {
			t.Errorf("%s: %+v", name, s)
		}
		// Its own store only, and none of the agent's data folder (secret values) through agents/atlas.
		if !slices.Contains(s.Volumes, p.StoreDir+":/store/atlas") || !slices.Equal(s.Tmpfs, []string{"/app/agents/atlas/data"}) {
			t.Errorf("%s: store %v, tmpfs %v", name, s.Volumes, s.Tmpfs)
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
	for _, s := range []string{"agent", "nap"} {
		if got := spec.Services[s].Restart; got != "unless-stopped" {
			t.Errorf("%s restart = %q, want unless-stopped", s, got)
		}
	}
}

func TestPortConcurrentAllocationsAreDistinct(t *testing.T) {
	root := t.TempDir()
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}
	got := make([]int, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			p, err := Port(root, id)
			if err != nil {
				t.Error(err)
			}
			got[i] = p
		}(i, id)
	}
	wg.Wait()
	seen := map[int]bool{}
	for i, p := range got {
		if seen[p] {
			t.Fatalf("port %d handed out twice: %v", p, got)
		}
		seen[p] = true
		if again, _ := Port(root, ids[i]); again != p {
			t.Errorf("%s: %d then %d (an entry was lost)", ids[i], p, again)
		}
	}
}

func TestPortsSkipOtherInstancesAndBusyPorts(t *testing.T) {
	t.Setenv("STORMO_HOME", t.TempDir())
	defer func(f func(int) bool) { portFree = f }(portFree)
	portFree = func(p int) bool { return p != PortBase+1 }
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "stormo.yaml"), []byte("name: Other\n"), 0o644)
	os.MkdirAll(filepath.Join(other, ".swarm"), 0o755)
	os.WriteFile(filepath.Join(other, ".swarm", "ports.json"), []byte(`{"x": 18642}`), 0o644)
	os.MkdirAll(filepath.Join(os.Getenv("STORMO_HOME"), "instances"), 0o700)
	os.WriteFile(filepath.Join(os.Getenv("STORMO_HOME"), "instances", instance.ClaimID(other)+".json"),
		[]byte(`{"root": "`+other+`", "name": "Other", "corePort": 18600}`), 0o600)
	if got, err := Port(t.TempDir(), "atlas"); err != nil || got != PortBase+2 {
		t.Errorf("got %d %v, want %d (18642 is the other instance's, 18643 is busy)", got, err, PortBase+2)
	}
}

func TestComposeGivesTheSidecarItsLink(t *testing.T) {
	inst, err := instance.Load("../../examples/minimal")
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(inst.Root, "atlas", inst.Names.Secret)
	if err != nil {
		t.Fatal(err)
	}
	p := Paths{Root: inst.Root, BaselineDir: "/b", EnvFile: "/x/agent.env", StoreDir: "/s", SharedDir: "/w",
		CoreURL: "http://host.docker.internal:18600", LinkEnvFile: "/x/link.env"}
	spec := Render(a, hermes.New(inst), p, 18642, 120, "stormo-sidecar:test")
	nap := spec.Services["nap"]
	if !slices.Equal(nap.EnvFile, []string{"/x/link.env"}) || nap.Environment["SWARM_CORE_URL"] != p.CoreURL ||
		nap.Environment["SWARM_AGENT_API"] != "http://agent:8642" || nap.Environment["SWARM_LINK_DIR"] != "/link" || !slices.Contains(nap.Volumes, "link:/link") {
		t.Errorf("nap: %+v", nap)
	}
	if spec.Volumes["link"] == nil {
		t.Error("no link volume")
	}
	// The agent's own env file is never the sidecar's, and its core calls go to the relay.
	if slices.Contains(nap.EnvFile, p.EnvFile) {
		t.Error("the sidecar got the agent's secrets")
	}
	if got := spec.Services["agent"].Environment["SWARM_CORE_URL"]; got != "http://nap:18690" {
		t.Errorf("agent SWARM_CORE_URL %q", got)
	}
	// Without link secrets the sidecar is as before.
	p.LinkEnvFile = ""
	if nap := Render(a, hermes.New(inst), p, 18642, 120, "x").Services["nap"]; len(nap.EnvFile) != 0 || nap.Environment["SWARM_CORE_URL"] != "" {
		t.Errorf("link off: %+v", nap)
	}
}
