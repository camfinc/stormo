package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/camfinc/stormo/internal/needletail/runtimelearn"
	"github.com/camfinc/stormo/internal/needletail/skills"
)

func TestSkillArchiveNapRestorePreservesPrivateModesAndNoResurrection(t *testing.T) {
	w := newLearningWorld(t)
	home := filepath.Join(w.tmp, "archive-home-1")
	w.rehydrate(home)
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	lib := &skills.Library{Home: home, Now: func() time.Time { return at }}
	s, err := lib.Create("archive-fixture", "", "---\nname: archive-fixture\ndescription: Acme fixture procedure\n---\nCheck a fixture.\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = lib.WriteFile(s.Dir, "scripts/check.sh", "#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(filepath.Join(home, "skills", s.Dir, "scripts/check.sh"), 0751); err != nil {
		t.Fatal(err)
	}
	at = at.Add(40 * 24 * time.Hour)
	rev, err := lib.BundleRevision(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	root := runtimelearn.Hash("archive-owner")
	receipt, err := lib.Archive(context.Background(), skills.ArchiveRequest{Root: root, Audience: "agent", Name: s.Dir, ExpectedRevision: rev}, skills.MaintenancePolicy{StaleAfter: 14 * 24 * time.Hour, ArchiveAfter: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	nap := w.nap(home, "archive-1", at)
	for _, f := range nap.Files {
		if strings.HasPrefix(f.Path, "skills/"+s.Dir+"/") {
			t.Fatal("archived skill remained in active snapshot")
		}
		if strings.HasPrefix(f.Path, skills.ArchiveDir+"/") && string(f.Class) != "raw" {
			t.Fatal("archive became non-private snapshot data")
		}
	}
	home2 := filepath.Join(w.tmp, "archive-home-2")
	w.rehydrate(home2)
	if _, err = os.Stat(filepath.Join(home2, "skills", s.Dir)); !os.IsNotExist(err) {
		t.Fatal("archived skill resurrected")
	}
	archivedScript := filepath.Join(home2, skills.ArchiveDir, receipt.ID, "scripts/check.sh")
	st, err := os.Stat(archivedScript)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("restored archive data must be private: %v", err)
	}
	restored := &skills.Library{Home: home2, Now: func() time.Time { return at }}
	if _, err = restored.ScopedRestoreArchive(context.Background(), root, "agent", receipt.ID); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(filepath.Join(home2, "skills", s.Dir, "scripts/check.sh"))
	if err != nil || st.Mode().Perm() != 0751 {
		t.Fatalf("script mode not restored: %v", err)
	}
	if got, e := restored.BundleRevision(s.Dir); e != nil || got != rev {
		t.Fatalf("bundle drift after restore %s != %s: %v", got, rev, e)
	}
}
