package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/messaging"
)

func TestStagePatchScopedImmutableReview(t *testing.T) {
	l := library(t)
	rev, err := l.BundleRevision("solo")
	if err != nil {
		t.Fatal(err)
	}
	r := PatchRequest{Root: "session-root", Audience: "private", Name: "solo", ExpectedRevision: rev, OldText: "A top-level skill.", NewText: "A clearer skill."}
	before, _ := os.ReadFile(filepath.Join(l.Home, "skills/solo/SKILL.md"))
	p, err := l.StagePatch(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	again, err := l.StagePatch(context.Background(), r)
	if err != nil || p.ID != again.ID {
		t.Fatalf("dedupe failed: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(l.Home, "skills/solo/SKILL.md"))
	if string(after) != string(before) {
		t.Fatal("proposal changed live skill")
	}
	for _, scope := range [][2]string{{"other", "private"}, {"session-root", "public"}} {
		list, err := l.ListPatches(scope[0], scope[1])
		if err != nil || len(list) != 0 {
			t.Fatalf("scope leak: %v", err)
		}
	}
	list, err := l.ListPatches("session-root", "private")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v", err)
	}
	info, err := os.Stat(filepath.Join(l.Home, ProposalsFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("proposal store not private")
	}
	put(t, l.Home, "skills/solo/reference.md", "changed bundle")
	if _, err := l.StagePatch(context.Background(), r); err == nil {
		t.Fatal("supporting-file revision drift accepted")
	}
}

func TestStagePatchRefusals(t *testing.T) {
	l := library(t)
	rev, _ := l.BundleRevision("solo")
	r := PatchRequest{Root: "root", Audience: "private", Name: "solo", ExpectedRevision: rev, OldText: "A top-level skill.", NewText: "A secret skill."}
	l.Check = func(string) error { return errors.New("secret") }
	if _, err := l.StagePatch(context.Background(), r); err == nil {
		t.Fatal("secret accepted")
	}
	l.Check = nil
	stale := r
	stale.ExpectedRevision = "obsolete"
	if _, err := l.StagePatch(context.Background(), stale); err == nil {
		t.Fatal("stale revision accepted")
	}
	if _, err := l.StagePatch(messaging.WithUntrusted(context.Background(), "marked"), r); err == nil {
		t.Fatal("quarantine staged persistent proposal")
	}
	r.OldText = "absent"
	if _, err := l.StagePatch(context.Background(), r); err == nil {
		t.Fatal("nonmatching patch accepted")
	}
	put(t, l.Home, ProposalsFile, "{")
	r.OldText = "A top-level skill."
	if _, err := l.StagePatch(context.Background(), r); err == nil {
		t.Fatal("corrupt store overwritten")
	}
}

func TestStagePatchRechecksAfterValidation(t *testing.T) {
	l := library(t)
	rev, _ := l.BundleRevision("solo")
	l.Check = func(string) error {
		put(t, l.Home, "skills/solo/reference.md", "concurrent supporting change")
		return nil
	}
	_, err := l.StagePatch(context.Background(), PatchRequest{Root: "root", Audience: "private", Name: "solo", ExpectedRevision: rev, OldText: "A top-level skill.", NewText: "A clearer skill."})
	if err == nil {
		t.Fatal("bundle changed during validation was staged")
	}
	if _, err := os.Stat(filepath.Join(l.Home, ProposalsFile)); !os.IsNotExist(err) {
		t.Fatal("refused candidate persisted")
	}
}

func TestBundleRevisionRefusesSymlink(t *testing.T) {
	l := library(t)
	if err := os.Symlink("/tmp", filepath.Join(l.Home, "skills/solo/link")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.BundleRevision("solo"); err == nil {
		t.Fatal("linked bundle accepted")
	}
}

func TestBundleRevisionIncludesExecutableMode(t *testing.T) {
	l := library(t)
	p := filepath.Join(l.Home, "skills/solo/script.sh")
	put(t, l.Home, "skills/solo/script.sh", "echo hello")
	before, err := l.BundleRevision("solo")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0755); err != nil {
		t.Fatal(err)
	}
	after, err := l.BundleRevision("solo")
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("executable mode omitted from bundle revision")
	}
}

func TestStagePatchRefusesQuarantineDuringValidation(t *testing.T) {
	l := library(t)
	rev, _ := l.BundleRevision("solo")
	ctx := messaging.WithQuarantine(context.Background(), nil)
	l.Check = func(string) error { return messaging.MarkUntrusted(ctx, "marked during validation") }
	_, err := l.StagePatch(ctx, PatchRequest{Root: "root", Audience: "private", Name: "solo", ExpectedRevision: rev, OldText: "A top-level skill.", NewText: "A clearer skill."})
	if err == nil {
		t.Fatal("quarantine during validation persisted proposal")
	}
	if _, err := os.Stat(filepath.Join(l.Home, ProposalsFile)); !os.IsNotExist(err) {
		t.Fatal("quarantined proposal persisted")
	}
}
