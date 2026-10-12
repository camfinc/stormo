package instance

import (
	"os"
	"path/filepath"
	"testing"
)

func scratchInstance(t *testing.T, name string) *Instance {
	t.Helper()
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "stormo.yaml"), []byte("name: "+name+"\n"), 0o644)
	return &Instance{Root: root, Name: name}
}

func TestCorePortIsKeptAndNeverAnotherInstances(t *testing.T) {
	t.Setenv("STORMO_HOME", t.TempDir())
	t.Setenv("SWARM_CORE_PORT", "")
	a := scratchInstance(t, "A")
	pa := a.CorePort()
	if pa < DefaultCorePort {
		t.Fatalf("A got %d", pa)
	}
	if again := a.CorePort(); again != pa {
		t.Errorf("A moved from %d to %d", pa, again)
	}
	b := scratchInstance(t, "B")
	if pb := b.CorePort(); pb == pa {
		t.Errorf("B took A's port %d", pa)
	}
	if len(Claims()) != 2 {
		t.Errorf("claims: %v", Claims())
	}
	// A deleted instance's claim is dropped.
	os.Remove(filepath.Join(a.Root, "stormo.yaml"))
	if len(Claims()) != 1 {
		t.Errorf("stale claim kept: %v", Claims())
	}
}

func TestCorePortSettingAndEnvWin(t *testing.T) {
	t.Setenv("STORMO_HOME", t.TempDir())
	t.Setenv("SWARM_CORE_PORT", "")
	i := scratchInstance(t, "C")
	i.CorePortSetting = 19123
	if p := i.CorePort(); p != 19123 {
		t.Errorf("core.port: %d", p)
	}
	t.Setenv("SWARM_CORE_PORT", "19200")
	if p := i.CorePort(); p != 19200 {
		t.Errorf("SWARM_CORE_PORT: %d", p)
	}
}
