package fsx

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The runtime's writes below a root the agent's commands share never go through a symlink: a
// directory swapped for one (to the runtime's cron/, say) makes the write fail instead.
func TestBelowNeverFollows(t *testing.T) {
	home := t.TempDir()
	tmp := filepath.Join(home, TmpDir)
	cron := filepath.Join(home, "cron")
	for _, d := range []string{cron, filepath.Join(home, "skills", "real")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(cron, "jobs.json"), []byte(`{"jobs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomicBelow(home, "skills/new/tools/x.luau", tmp, []byte("ok"), FilePerm); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, "skills", "new", "tools", "x.luau"))
	if err != nil || info.Mode().Perm()&0o020 == 0 {
		t.Fatalf("written %v %v", info, err)
	}
	if err := os.Symlink(cron, filepath.Join(home, "skills", "evil")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(cron, "jobs.json"), filepath.Join(home, "skills", "real", "jobs.json")); err != nil {
		t.Fatal(err)
	}
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, ErrLink) {
			t.Errorf("%s: %v, want ErrLink", what, err)
		}
	}
	check("atomic write through a dir link", WriteAtomicBelow(home, "skills/evil/jobs.json", tmp, []byte("pwned"), FilePerm))
	_, err = OpenFileBelow(home, "skills/evil/jobs.json", os.O_WRONLY|os.O_CREATE, FilePerm, true)
	check("open through a dir link", err)
	_, err = OpenFileBelow(home, "skills/real/jobs.json", os.O_WRONLY, FilePerm, false)
	check("open of a final link", err)
	check("remove through a dir link", RemoveBelow(home, "skills/evil/jobs.json", false))
	check("rename through a dir link", RenameOut(home, "skills/evil/jobs.json", filepath.Join(tmp, "gone")))
	if b, _ := os.ReadFile(filepath.Join(cron, "jobs.json")); string(b) != `{"jobs":[]}` {
		t.Fatalf("cron/jobs.json changed: %s", b)
	}
	// an atomic write replaces a final link, never writes through it
	if err := WriteAtomicBelow(home, "skills/real/jobs.json", tmp, []byte("mine"), FilePerm); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(cron, "jobs.json")); string(b) != `{"jobs":[]}` {
		t.Fatalf("cron/jobs.json changed: %s", b)
	}
	if err := RemoveBelow(home, "skills/real/jobs.json", false); err != nil {
		t.Fatal(err)
	}
	if err := RenameOut(home, "skills/new", filepath.Join(tmp, "gone")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "gone", "tools", "x.luau")); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"../x", "/abs", "a/../b", ""} {
		if err := WriteAtomicBelow(home, bad, tmp, nil, FilePerm); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// WalkBelow lists by descriptor: a symlink is passed as itself and never entered, and a
// directory swapped for a symlink after it was listed (here, from inside the walk, as a command
// racing it would) is not entered either, where filepath.WalkDir reads what the link points to.
func TestWalkBelowFollowsNothing(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"skills/a/SKILL.md", "skills/b/x/file.txt", "sessions/secret.jsonl"} {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "sessions"), filepath.Join(root, "skills", "link")); err != nil {
		t.Fatal(err)
	}
	var seen []string
	err := WalkBelow(root, "skills", func(rel string, d fs.DirEntry) error {
		seen = append(seen, rel)
		if rel == "skills/b/x" { // swapped once listed, before it is entered
			if err := os.Rename(filepath.Join(root, "skills", "b", "x"), filepath.Join(root, "skills", "b", "old")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "sessions"), filepath.Join(root, "skills", "b", "x")); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range seen {
		if strings.Contains(s, "secret") {
			t.Errorf("the walk read the runtime's sessions/ through a link: %v", seen)
		}
	}
	if !slices.Contains(seen, "skills/a/SKILL.md") || !slices.Contains(seen, "skills/link") {
		t.Errorf("seen %v", seen)
	}
	if err := WalkBelow(root, "skills/link", func(string, fs.DirEntry) error { return nil }); !errors.Is(err, ErrLink) {
		t.Errorf("a walk started at a symlink: %v", err)
	}
}
