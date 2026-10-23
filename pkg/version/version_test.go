package version

import (
	"os"
	"path/filepath"
	"testing"
)

// A binary built away from its checkout (the macOS app's copy) finds it through BuiltFrom, and
// only while the checkout is still there.
func TestSourceFallsBackToBuiltFrom(t *testing.T) {
	t.Setenv("STORMO_SRC", "")
	saved := BuiltFrom
	t.Cleanup(func() { BuiltFrom = saved })

	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module github.com/camfinc/stormo\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	BuiltFrom = checkout
	if got := Source(); got != checkout {
		t.Fatalf("Source() = %q, want BuiltFrom %q", got, checkout)
	}

	t.Setenv("STORMO_SRC", "/elsewhere")
	if got := Source(); got != "/elsewhere" {
		t.Fatalf("STORMO_SRC must win: Source() = %q", got)
	}
	t.Setenv("STORMO_SRC", "")

	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "go.mod"), []byte("module example.com/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	BuiltFrom = other
	if got := Source(); got != "" {
		t.Fatalf("a directory without the engine's go.mod is not a source: Source() = %q", got)
	}
	BuiltFrom = filepath.Join(checkout, "gone")
	if got := Source(); got != "" {
		t.Fatalf("a removed checkout is not a source: Source() = %q", got)
	}
}
