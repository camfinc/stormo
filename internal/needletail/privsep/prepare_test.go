package privsep

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/memory"
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
// group-writable, the runtime's directories made and private (the hot memory files readable by
// the group, the archive not),
// the home sticky, the root files the runtime's, nothing followed through a symlink, a runtime
// name that is not a directory moved aside.
func TestPrepare(t *testing.T) {
	home := t.TempDir()
	put(t, filepath.Join(home, "skills", "crm", "SKILL.md"), "x", 0o644)
	put(t, filepath.Join(home, "skills", "crm", "scripts", "run.sh"), "x", 0o755)
	put(t, filepath.Join(home, "scripts", "job.py"), "x", 0o644)
	put(t, filepath.Join(home, "skills", "crm", "ro.txt"), "x", 0o444) // read-only stays read-only
	// owner-only stays owner-only: ssh refuses a key the group may read
	put(t, filepath.Join(home, ".ssh", "id_ed25519"), "key", 0o600)
	if err := os.Chmod(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(home, "work", "notes.txt"), "x", 0o640)
	put(t, filepath.Join(home, "cron", "jobs.json"), "{}", 0o644)
	put(t, filepath.Join(home, "sessions", "a.jsonl"), "x", 0o644)
	put(t, filepath.Join(home, "memories", "MEMORY.md"), "x", 0o644)
	put(t, filepath.Join(home, "memories", "USER.md"), "x", 0o644)
	put(t, filepath.Join(home, "memories", "RECORDS.md"), "x", 0o644) // as rehydrate restores it
	put(t, filepath.Join(home, "needletail.yaml"), "x", 0o644)
	outside := filepath.Join(t.TempDir(), "outside.txt") // a symlink's target: never changed
	put(t, outside, "secret", 0o600)
	put(t, filepath.Join(home, "logs"), "a file where a directory goes", 0o644)
	if err := os.Symlink(outside, filepath.Join(home, "skills", "crm", "link")); err != nil {
		t.Fatal(err)
	}
	var logs []string
	u := Users{RuntimeUID: os.Getuid(), RuntimeGID: os.Getgid(), ExecUID: os.Getuid()}
	if err := Prepare(home, u, "acme-state", func(s string) { logs = append(logs, s) }); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]fs.FileMode{
		".":                         fs.ModeDir | fs.ModeSticky | fs.ModeSetgid | 0o775,
		"skills":                    fs.ModeDir | 0o775,
		"skills/crm/SKILL.md":       0o664,
		"skills/crm/scripts/run.sh": 0o775,
		"scripts/job.py":            0o664,
		"skills/crm/ro.txt":         0o444,
		".ssh":                      fs.ModeDir | 0o700,
		".ssh/id_ed25519":           0o600,
		"work":                      fs.ModeDir | 0o775,
		"work/notes.txt":            0o660,
		"cron":                      fs.ModeDir | 0o700,
		"cron/jobs.json":            0o600,
		"sessions/a.jsonl":          0o600,
		"state/needletail":          fs.ModeDir | 0o700,
		"logs/audit":                fs.ModeDir | 0o700,
		".swarm":                    fs.ModeDir | 0o700,
		".needletail-tmp":           fs.ModeDir | 0o700,
		"memories":                  fs.ModeDir | 0o750,
		"memories/MEMORY.md":        0o640,
		"memories/RECORDS.md":       0o600, // the archive: the runtime's alone
		"memories/USER.md":          0o640,
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
	if err := Prepare(home, u, "acme-state", func(string) {}); err == nil || !strings.Contains(err.Error(), "SOUL.md") {
		t.Errorf("a symlinked SOUL.md: %v", err)
	}
	if got := mode(t, filepath.Join(home, "cron", "jobs.json")); got != 0o600 {
		t.Errorf("the link's target changed: %v", got)
	}
}

// The file Prepare keeps private in memories/ is the memory's archive.
func TestPrivateFilesIsTheArchive(t *testing.T) {
	if !slices.Contains(PrivateFiles, memory.RecordsFile) || !slices.Contains(ReadOnly, path.Dir(memory.RecordsFile)) {
		t.Errorf("PrivateFiles %v, ReadOnly %v, the archive %s", PrivateFiles, ReadOnly, memory.RecordsFile)
	}
}

// Enter splits only as root on Linux; otherwise the runtime runs as one user.
func TestEnterOffWhenNotRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root: the split would run")
	}
	var logs []string
	s, err := Enter(t.TempDir(), "acme-state", func(l string) { logs = append(logs, l) })
	if err != nil || s.Exec != nil || s.Off == "" {
		t.Fatalf("split %+v %v", s, err)
	}
}

// What a command made owner-only inside the trees the runtime reads and writes for the agent
// (skills/, scripts/, the manifest's state dir: a mktemp file or directory moved there) is given
// the group's access, so the runtime is never locked out of it for good; owner-only entries
// elsewhere (~/.ssh, ~/.gnupg) keep their modes.
func TestPrepareWorkspaceTreesStayReachable(t *testing.T) {
	home := t.TempDir()
	put(t, filepath.Join(home, "skills", "crm", "priv", "tmp.Xa12"), "x", 0o600)
	put(t, filepath.Join(home, "scripts", "job.sh"), "x", 0o700)
	put(t, filepath.Join(home, "acme-state", "crm", "cursor"), "x", 0o600)
	put(t, filepath.Join(home, ".gnupg", "key"), "x", 0o600)
	put(t, filepath.Join(home, "work", "private", "notes"), "x", 0o600)
	for _, d := range []string{"skills/crm/priv", "acme-state/crm", ".gnupg", "work/private"} {
		if err := os.Chmod(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	u := Users{RuntimeUID: os.Getuid(), RuntimeGID: os.Getgid(), ExecUID: os.Getuid()}
	if err := Prepare(home, u, "acme-state", func(string) {}); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]fs.FileMode{
		"skills/crm/priv":          fs.ModeDir | 0o770,
		"skills/crm/priv/tmp.Xa12": 0o660,
		"scripts/job.sh":           0o770,
		"acme-state/crm":           fs.ModeDir | 0o770,
		"acme-state/crm/cursor":    0o660,
		".gnupg":                   fs.ModeDir | 0o700,
		".gnupg/key":               0o600,
		"work/private":             fs.ModeDir | 0o700,
		"work/private/notes":       0o600,
	} {
		if got := mode(t, filepath.Join(home, rel)); got != want {
			t.Errorf("%s: %v, want %v", rel, got, want)
		}
	}
}

// The commands' temporary directory (TMPDIR): made when missing, the runtime's and group-writable
// (setgid), what a command left there kept as the workspace's; a symlink holding the name (to
// /tmp, say) moved aside, so TMPDIR never leads out of the home.
func TestPrepareTempDir(t *testing.T) {
	u := Users{RuntimeUID: os.Getuid(), RuntimeGID: os.Getgid(), ExecUID: os.Getuid()}
	home := t.TempDir()
	if err := Prepare(home, u, "acme-state", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, filepath.Join(home, Temp)); got != fs.ModeDir|fs.ModeSetgid|0o770 {
		t.Errorf("made %s: %v", Temp, got)
	}
	put(t, filepath.Join(home, Temp, "tmp.abc"), "x", 0o600)
	if err := Prepare(home, u, "acme-state", func(string) {}); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, filepath.Join(home, Temp, "tmp.abc")); got != 0o600 {
		t.Errorf("a command's temp file: %v", got)
	}
	outside := t.TempDir()
	if err := os.RemoveAll(filepath.Join(home, Temp)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(home, Temp)); err != nil {
		t.Fatal(err)
	}
	var logs []string
	if err := Prepare(home, u, "acme-state", func(s string) { logs = append(logs, s) }); err != nil {
		t.Fatal(err)
	}
	if got := mode(t, filepath.Join(home, Temp)); got != fs.ModeDir|fs.ModeSetgid|0o770 {
		t.Errorf("%s after a symlink: %v", Temp, got)
	}
	if moved, _ := filepath.Glob(filepath.Join(home, Temp+".moved-*")); len(moved) != 1 || len(logs) != 1 {
		t.Errorf("moved %v logs %v", moved, logs)
	}
	if got := mode(t, outside); got != fs.ModeDir|0o700 && got != fs.ModeDir|0o755 {
		t.Errorf("the link's target changed: %v", got)
	}
}
