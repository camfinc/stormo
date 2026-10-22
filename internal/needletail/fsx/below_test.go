package fsx

import (
	"errors"
	"os"
	"path/filepath"
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
