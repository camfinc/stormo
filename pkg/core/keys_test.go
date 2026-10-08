package core

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestAgentKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.local.yaml")
	write := func(body string) {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("local:\n  agents:\n    atlas:\n      SWARM_CORE_KEY: atlas-core-key-0123456789\n    scout:\n      SWARM_CORE_KEY: short\n")
	k := NewAgentKeys(path)
	if a, ok := k.AgentFor("Bearer atlas-core-key-0123456789"); !ok || a != "atlas" {
		t.Fatalf("got %q %v", a, ok)
	}
	if a, ok := k.AgentFor("bearer   atlas-core-key-0123456789"); !ok || a != "atlas" {
		t.Fatalf("case/space: %q %v", a, ok)
	}
	for _, h := range []string{"", "atlas-core-key-0123456789", "Bearer short", "Bearer nope-nope-nope-nope"} {
		if _, ok := k.AgentFor(h); ok {
			t.Errorf("%q must not resolve", h)
		}
	}
	// A new key works without restarting the core.
	time.Sleep(10 * time.Millisecond)
	write("local:\n  agents:\n    atlas:\n      SWARM_CORE_KEY: atlas-core-key-0123456789\n    nova:\n      SWARM_CORE_KEY: nova-core-key-0123456789\n")
	if got := k.Agents(); !reflect.DeepEqual(got, []string{"atlas", "nova"}) {
		t.Fatalf("agents %v", got)
	}
}
