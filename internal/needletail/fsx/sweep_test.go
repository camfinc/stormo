package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

// The boot sweep empties the temp dir by descriptor: a read-only directory is opened (never
// through a symlink) and made writable through its descriptor, and a symlink is removed as
// itself, so nothing it points to (the home's root, a shared directory) is changed or emptied.
func TestSweepNeverFollows(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	home := t.TempDir()
	outside := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o2755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(home, TmpDir, "deleted-1", "skill")
	ro := filepath.Join(gone, "data")
	if err := os.MkdirAll(ro, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(ro, "a.md"), filepath.Join(gone, "b.md")} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []string{filepath.Join(ro, "up"), filepath.Join(gone, "shared"), filepath.Join(gone, "home")} {
		target := outside
		if filepath.Base(l) == "home" {
			target = home
		}
		if err := os.Symlink(target, l); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(ro, 0o555); err != nil {
		t.Fatal(err)
	}
	homeMode, sharedMode := mode(t, home), mode(t, outside)
	if err := Sweep(home); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, TmpDir)); len(entries) != 0 {
		t.Errorf("left in the temp dir: %v", entries)
	}
	if m := mode(t, outside); m != sharedMode {
		t.Errorf("the shared directory's mode changed: %v, was %v", m, sharedMode)
	}
	if m := mode(t, home); m != homeMode {
		t.Errorf("the home's mode changed: %v, was %v", m, homeMode)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Errorf("a file behind a symlink was removed: %v", err)
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}
