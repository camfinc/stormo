package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRenameBelowExclusiveAndParents(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"source", "dest", "other"} {
		if e := os.Mkdir(filepath.Join(root, p), 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e := RenameBelow(root, "source", "dest"); e == nil {
		t.Fatal("clobbered destination")
	}
	if e := os.Symlink(filepath.Join(root, "other"), filepath.Join(root, "alias")); e != nil {
		t.Fatal(e)
	}
	if e := RenameBelow(root, "source", "alias/moved"); e == nil {
		t.Fatal("followed parent alias")
	}
	if e := RenameBelow(root, "alias", "moved"); e == nil {
		t.Fatal("moved source alias")
	}
	if e := RenameBelow(root, "source", "other/moved"); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(root, "other/moved")); e != nil {
		t.Fatal(e)
	}
}
