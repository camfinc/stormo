package privsep

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mode(t *testing.T, p string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func put(t *testing.T, p, body string, perm fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
}

// Prepare's layout, with the test's own uid standing in for both users (only root can give files
// to others; the Docker check in docs/needletail.md §7 runs it as root): the workspace
// group-writable, the runtime's directories made and private (memories readable by the group),
// the home sticky, the root files the runtime's, nothing followed through a symlink, a runtime
// name that is not a directory moved aside.
func TestPrepare(t *testing.T) {
	home := t.TempDir()
	put(t, filepath.Join(home, "skills", "crm", "SKILL.md"), "x", 0o644)
	put(t, filepath.Join(home, "skills", "crm", "scripts", "run.sh"), "x", 0o755)
	put(t, filepath.Join(home, "scripts", "job.py"), "x", 0o644)
	put(t, filepath.Join(home, "cron", "jobs.json"), "{}", 0o644)
	put(t, filepath.Join(home, "sessions", "a.jsonl"), "x", 0o644)
	put(t, filepath.Join(home, "memories", "MEMORY.md"), "x", 0o644)
	put(t, filepath.Join(home, "needletail.yaml"), "x", 0o644)
	outside := filepath.Join(t.TempDir(), "outside.txt") // a symlink's target: never changed
	put(t, outside, "secret", 0o600)
	put(t, filepath.Join(home, "logs"), "a file where a directory goes", 0o644)
	if err := os.Symlink(outside, filepath.Join(home, "skills", "crm", "link")); err != nil {
		t.Fatal(err)
	}
	var logs []string
	u := Users{RuntimeUID: os.Getuid(), RuntimeGID: os.Getgid(), ExecUID: os.Getuid()}
	if err := Prepare(home, u, func(s string) { logs = append(logs, s) }); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]fs.FileMode{
		".":                         fs.ModeDir | fs.ModeSticky | fs.ModeSetgid | 0o775,
		"skills":                    fs.ModeDir | 0o775,
		"skills/crm/SKILL.md":       0o664,
		"skills/crm/scripts/run.sh": 0o775,
		"scripts/job.py":            0o664,
		"cron":                      fs.ModeDir | 0o700,
		"cron/jobs.json":            0o600,
		"sessions/a.jsonl":          0o600,
		"state/needletail":          fs.ModeDir | 0o700,
		"logs/audit":                fs.ModeDir | 0o700,
		".swarm":                    fs.ModeDir | 0o700,
		".needletail-tmp":           fs.ModeDir | 0o700,
		"memories":                  fs.ModeDir | 0o750,
		"memories/MEMORY.md":        0o640,
		"needletail.yaml":           0o600,
		"SOUL.md":                   0o644,
		"skills/crm/link":           fs.ModeSymlink | mode(t, filepath.Join(home, "skills", "crm", "link")).Perm(),
	} {
		if got := mode(t, filepath.Join(home, rel)); got != want {
			t.Errorf("%s: %v, want %v", rel, got, want)
		}
	}
	if got := mode(t, outside); got != 0o600 {
		t.Errorf("a symlink's target changed: %v", got)
	}
	moved, _ := filepath.Glob(filepath.Join(home, "logs.moved-*"))
	if len(moved) != 1 || len(logs) != 1 || !strings.Contains(logs[0], "logs was not a directory; moved aside") {
		t.Errorf("moved %v logs %v", moved, logs)
	}
	// a root file that is a symlink fails the boot
	if err := os.Remove(filepath.Join(home, "SOUL.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "cron", "jobs.json"), filepath.Join(home, "SOUL.md")); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(home, u, func(string) {}); err == nil || !strings.Contains(err.Error(), "SOUL.md") {
		t.Errorf("a symlinked SOUL.md: %v", err)
	}
	if got := mode(t, filepath.Join(home, "cron", "jobs.json")); got != 0o600 {
		t.Errorf("the link's target changed: %v", got)
	}
}

// Enter splits only as root on Linux; otherwise the runtime runs as one user.
func TestEnterOffWhenNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root: the split would run")
	}
	var logs []string
	s, err := Enter(t.TempDir(), func(l string) { logs = append(logs, l) })
	if err != nil || s.Exec != nil || s.Off == "" {
		t.Fatalf("split %+v %v", s, err)
	}
}
