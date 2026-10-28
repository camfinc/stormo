package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func staged(t *testing.T, l *Library, a string) SkillPatch {
	t.Helper()
	rev, e := l.BundleRevision("solo")
	if e != nil {
		t.Fatal(e)
	}
	p, e := l.StagePatch(context.Background(), PatchRequest{Root: "root", Audience: a, Name: "solo", ExpectedRevision: rev, OldText: "A top-level skill.", NewText: "An improved skill."})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestOwnerPatchActivationRollbackAndCrash(t *testing.T) {
	l := library(t)
	p := staged(t, l, "agent")
	q := PatchDecisionRequest{Root: p.Root, Audience: p.Audience, ID: p.ID, Action: "approve", ExpectedRevision: p.ExpectedRevision}
	r, e := l.DecidePatch(context.Background(), q)
	if e != nil {
		t.Fatal(e)
	}
	if r.Status != "applied" || r.Before == r.After {
		t.Fatal(r)
	}
	rev, _ := l.BundleRevision("solo")
	if rev != r.AfterRevision {
		t.Fatal("future revision mismatch")
	}
	ds, _ := l.readDecisions()
	ds.Receipts[0].Status = "applying"
	if e = l.savePrivate(DecisionsFile, ds); e != nil {
		t.Fatal(e)
	}
	v, e := l.ReviewPatches("root", "agent")
	if e != nil || v[0].Status != "applied" {
		t.Fatalf("crash reconciliation: %v %v", v, e)
	}
	q.Action = "rollback"
	q.ExpectedRevision = rev
	r, e = l.DecidePatch(context.Background(), q)
	if e != nil || r.Status != "rolledback" {
		t.Fatalf("rollback: %v %v", r, e)
	}
	rev, _ = l.BundleRevision("solo")
	if rev != p.ExpectedRevision {
		t.Fatal("rollback bytes/mode changed")
	}
	q.Action = "approve"
	if _, e = l.DecidePatch(context.Background(), q); e == nil {
		t.Fatal("rolledback candidate resurrected")
	}
}
func TestPatchScopePrivateStaleAndRejection(t *testing.T) {
	for _, a := range []string{"private", "agent"} {
		l := library(t)
		p := staged(t, l, a)
		q := PatchDecisionRequest{Root: "other", Audience: a, ID: p.ID, Action: "approve", ExpectedRevision: p.ExpectedRevision}
		if _, e := l.DecidePatch(context.Background(), q); e == nil {
			t.Fatal("scope leak")
		}
		q.Root = "root"
		if a == "agent" {
			put(t, l.Home, "skills/solo/script.sh", "changed")
		}
		if _, e := l.DecidePatch(context.Background(), q); e == nil {
			t.Fatal("private or stale activation")
		}
		q.Action = "reject"
		if _, e := l.DecidePatch(context.Background(), q); e != nil {
			t.Fatal(e)
		}
		q.Action = "approve"
		if _, e := l.DecidePatch(context.Background(), q); e == nil {
			t.Fatal("rejection replay")
		}
	}
}
func TestArchivePreservesBundleAndConflicts(t *testing.T) {
	l := library(t)
	now := time.Now().UTC()
	l.Now = func() time.Time { return now }
	s, e := l.Create("old", "", "---\nname: old\ndescription: Example\n---\nExample")
	if e != nil {
		t.Fatal(e)
	}
	script := filepath.Join(l.Home, "skills/old/script.sh")
	if e = os.WriteFile(script, []byte("#!/bin/sh\necho example"), 0751); e != nil {
		t.Fatal(e)
	}
	l.Now = func() time.Time { return now.Add(31 * 24 * time.Hour) }
	rev, _ := l.BundleRevision(s.Dir)
	q := ArchiveRequest{Root: "root", Audience: "agent", Name: s.Dir, ExpectedRevision: rev}
	p := MaintenancePolicy{StaleAfter: 14 * 24 * time.Hour, ArchiveAfter: 30 * 24 * time.Hour, Protected: []string{"old"}}
	if _, e = l.Archive(context.Background(), q, p); e == nil {
		t.Fatal("protected archive")
	}
	p.Protected = nil
	r, e := l.Archive(context.Background(), q, p)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = l.Find("old"); e == nil {
		t.Fatal("still indexed")
	}
	// Nap/rehydrate resets Raw modes; the receipt must retain script executability.
	if err := filepath.WalkDir(filepath.Join(l.Home, ArchiveDir, r.ID), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if !d.IsDir() {
			return os.Chmod(p, 0644)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	l = &Library{Home: l.Home, Now: l.Now}

	put(t, l.Home, "skills/old/conflict.txt", "owner")
	if _, e = l.RestoreArchive(context.Background(), r.ID); e == nil {
		t.Fatal("restore overwrite")
	}
	os.RemoveAll(filepath.Join(l.Home, "skills/old"))
	if _, e = l.RestoreArchive(context.Background(), r.ID); e != nil {
		t.Fatal(e)
	}
	got, _ := l.BundleRevision("old")
	if got != rev {
		t.Fatal("bundle changed")
	}
	st, _ := os.Stat(script)
	if st.Mode().Perm() != 0751 {
		t.Fatal("mode lost")
	}
}

func TestArchiveWALRestoreReconciliationAndScope(t *testing.T) {
	l := library(t)
	now := time.Now().UTC()
	l.Now = func() time.Time { return now }
	_, e := l.Create("old", "", "---\nname: old\ndescription: Example\n---\nExample")
	if e != nil {
		t.Fatal(e)
	}
	l.Now = func() time.Time { return now.Add(31 * 24 * time.Hour) }
	rev, _ := l.BundleRevision("old")
	r, e := l.Archive(context.Background(), ArchiveRequest{Root: "root", Audience: "agent", Name: "old", ExpectedRevision: rev}, MaintenancePolicy{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = l.ScopedRestoreArchive(context.Background(), "other", "agent", r.ID); e == nil {
		t.Fatal("scope restore")
	}
	if _, e = l.ScopedRestoreArchive(context.Background(), "root", "agent", r.ID); e != nil {
		t.Fatal(e)
	}
	store, _ := l.readArchives()
	store.Archives[0].Status = "restoring"
	if e = l.savePrivate(ArchivesFile, store); e != nil {
		t.Fatal(e)
	}
	all, e := l.ListArchives()
	if e != nil || all[0].Status != "restored" {
		t.Fatalf("crash: %v %v", all, e)
	}
}
func TestUnknownDecisionAndMaintenanceStatePreserved(t *testing.T) {
	l := library(t)
	p := staged(t, l, "agent")
	bad := "{\"version\":99,\"receipts\":[]}"
	put(t, l.Home, DecisionsFile, bad)
	if _, e := l.DecidePatch(context.Background(), PatchDecisionRequest{Root: p.Root, Audience: p.Audience, ID: p.ID, Action: "reject"}); e == nil {
		t.Fatal("future state overwritten")
	}
	b, _ := os.ReadFile(filepath.Join(l.Home, DecisionsFile))
	if string(b) != bad {
		t.Fatal("changed state")
	}
	put(t, l.Home, MaintenanceFile, "{\"version\":99,\"proposals\":[]}")
	if _, e := l.ForceScanMaintenance(MaintenancePolicy{}); e == nil {
		t.Fatal("future scan overwritten")
	}
}

func TestArchiveRefusesHardlinkedModeRestoration(t *testing.T) {
	l := library(t)
	now := time.Now().UTC()
	l.Now = func() time.Time { return now }
	_, err := l.Create("old", "", "---\nname: old\ndescription: Example\n---\nExample")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(l.Home, "skills/old/script.sh")
	if err = os.WriteFile(source, []byte("example"), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(source, filepath.Join(l.Home, "outside-script")); err != nil {
		t.Fatal(err)
	}
	l.Now = func() time.Time { return now.Add(31 * 24 * time.Hour) }
	rev, _ := l.BundleRevision("old")
	if _, err = l.Archive(context.Background(), ArchiveRequest{Root: "root", Audience: "agent", Name: "old", ExpectedRevision: rev}, MaintenancePolicy{}); err == nil {
		t.Fatal("hardlinked bundle archived")
	}
	if _, err = l.Find("old"); err != nil {
		t.Fatal("failed archive removed skill")
	}
}
