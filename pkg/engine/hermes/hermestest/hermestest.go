// Package hermestest is the example instance with its agents on Hermes, for tests of Hermes'
// behaviour (and of what only a Hermes agent declares: a Telegram channel, Slack slash commands).
// The example instance (examples/minimal) runs needletail; this lays the agents it had on Hermes
// over a copy of it: their agent.yaml files and atlas's engine/hermes/config.yaml, under
// instance/agents. Everything else (units, skills, souls, personas, bridge, stormo.yaml) is the
// example's own, so the two never drift apart. Only tests import it.
package hermestest

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/camfinc/stormo"
)

//go:embed instance
var overlay embed.FS

// Instance writes the Hermes copy of the example instance into a new temporary directory and
// returns its root.
func Instance(t testing.TB) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "acme")
	if err := Write(root); err != nil {
		t.Fatal(err)
	}
	return root
}

// Write writes the Hermes copy of the example instance into root, which must not exist yet.
func Write(root string) error {
	if err := os.CopyFS(root, stormo.Example()); err != nil {
		return err
	}
	over, err := fs.Sub(overlay, "instance")
	if err != nil {
		return err
	}
	agents, err := fs.ReadDir(over, "agents")
	if err != nil {
		return err
	}
	// An overlaid agent's engine files are Hermes' alone: drop whatever engine dir the example has.
	for _, a := range agents {
		if err := os.RemoveAll(filepath.Join(root, "agents", a.Name(), "engine")); err != nil {
			return err
		}
	}
	return fs.WalkDir(over, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(over, p)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, body, 0o644)
	})
}
