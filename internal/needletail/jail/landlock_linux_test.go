//go:build linux

package jail

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRightsAreTheKernels(t *testing.T) {
	for _, c := range []struct {
		ours, kernel uint64
	}{
		{Execute, unix.LANDLOCK_ACCESS_FS_EXECUTE}, {WriteFile, unix.LANDLOCK_ACCESS_FS_WRITE_FILE},
		{ReadFile, unix.LANDLOCK_ACCESS_FS_READ_FILE}, {ReadDir, unix.LANDLOCK_ACCESS_FS_READ_DIR},
		{RemoveDir, unix.LANDLOCK_ACCESS_FS_REMOVE_DIR}, {RemoveFile, unix.LANDLOCK_ACCESS_FS_REMOVE_FILE},
		{MakeChar, unix.LANDLOCK_ACCESS_FS_MAKE_CHAR}, {MakeDir, unix.LANDLOCK_ACCESS_FS_MAKE_DIR},
		{MakeReg, unix.LANDLOCK_ACCESS_FS_MAKE_REG}, {MakeSock, unix.LANDLOCK_ACCESS_FS_MAKE_SOCK},
		{MakeFifo, unix.LANDLOCK_ACCESS_FS_MAKE_FIFO}, {MakeBlock, unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK},
		{MakeSym, unix.LANDLOCK_ACCESS_FS_MAKE_SYM}, {Refer, unix.LANDLOCK_ACCESS_FS_REFER},
		{Truncate, unix.LANDLOCK_ACCESS_FS_TRUNCATE}, {IoctlDev, unix.LANDLOCK_ACCESS_FS_IOCTL_DEV},
	} {
		if c.ours != c.kernel {
			t.Errorf("right %#x, kernel's %#x", c.ours, c.kernel)
		}
	}
}

// TestJailAsRoot is the jail for real: as root in a container (`GOOS=linux CGO_ENABLED=0 go test
// -c`, then run the binary as root with the compose hardening flags; docs/needletail.md §7) it
// starts a shell through the launcher as the exec user, as the terminal does, with a home and two
// shared layers, and another layer mounted but not granted; the shell reports what it could do.
// Skipped when not root or without Landlock.
func TestJailAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root: run the test binary in a container")
	}
	abi, err := ABI()
	if err != nil {
		t.Skipf("no Landlock: %v", err)
	}
	t.Logf("Landlock ABI %d", abi)
	base := t.TempDir()
	if err := os.Chmod(filepath.Dir(base), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	home, group, unit, other := filepath.Join(base, "home"), filepath.Join(base, "shared/group"), filepath.Join(base, "shared/sales"), filepath.Join(base, "shared/other")
	for _, d := range []string{home, group, unit, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o2775)
		if d == other {
			mode = 0o777 // the file modes would let the exec user in: only the jail keeps it out
		}
		if err := os.Chown(d, 10000, 10000); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(home, 0o3775); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(other, "secret.txt"), filepath.Join(group, "notes.md"), filepath.Join(home, "existing.txt")} {
		if err := os.WriteFile(f, []byte("data\n"), 0o666); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(f, 0o666)
	}
	outside := filepath.Join(os.TempDir(), fmt.Sprintf("nt-jail-outside-%d", os.Getpid()))
	if err := os.WriteFile(outside, []byte("outside\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(outside, 0o666)
	t.Cleanup(func() { os.Remove(outside) })
	self, _ := os.Executable()
	script := fmt.Sprintf(`cd %[1]q
try() { if (eval "$2") >/dev/null 2>&1; then echo "jail: $1: ok"; else echo "jail: $1: denied"; fi; }
try "write home" 'echo x > work.txt && mkdir -p a/b && echo y > a/b/f'
try "move across home dirs" 'mkdir -p c && mv a/b/f c/f'
try "truncate in home" ': > existing.txt'
try "fifo in home" 'mkfifo p && rm p'
try "write group" 'echo x > %[2]s/w.txt && cat %[2]s/notes.md'
try "write unit" 'echo x > %[3]s/w.txt'
try "mktemp in TMPDIR" 'f=$(mktemp) && case "$f" in %[1]s/.tmp/*) rm "$f";; *) false;; esac'
try "python3" 'python3 -c "import ssl, socket, tempfile; ssl.create_default_context(); socket.getaddrinfo(\"localhost\", \"https\"); tempfile.mkstemp()"'
try "git" 'git init -q repo && cd repo && git -c user.email=a@b -c user.name=a commit -q --allow-empty -m x && git log --oneline'
try "read /etc/passwd" 'cat /etc/passwd'
try "read /proc/self/status" 'grep -q NoNewPrivs:.1 /proc/self/status'
try "process substitution" 'cat <(echo hi)'
try "devices" 'echo x > /dev/null && head -c 4 /dev/urandom'
try "read other layer" 'cat %[4]s/secret.txt'
try "write other layer" 'echo x > %[4]s/w.txt'
try "list other layer" 'ls %[4]s'
try "write /tmp" 'echo x > /tmp/nt-jail-w'
try "read /tmp" 'cat %[5]s'
try "write /var/tmp" 'echo x > /var/tmp/nt-jail-w'
try "write /usr" 'echo x > /usr/nt-jail-w'
try "read /etc/shells" 'cat /etc/shells'
try "read /etc/shadow" 'cat /etc/shadow'
try "list /" 'ls /'
try "list /root" 'ls /root'
try "read the launcher" 'head -c 4 %[6]s'
try "list /opt" 'ls /opt'
try "run a program" 'test "$(/bin/echo hi)" = hi'
`, home, group, unit, other, outside, self)
	cmd := exec.Command(self, append(Spec{Home: home, Shared: []string{group, unit}}.Args()[1:], "/bin/bash", "bash", "--noprofile", "--norc", "-c", script)...)
	cmd.Env = []string{launchEnv + "=1", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + home,
		"TMPDIR=" + filepath.Join(home, TmpDir), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=*"}
	cmd.Dir = home
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: 10001, Gid: 10000, Groups: []uint32{}}}
	out, err := cmd.CombinedOutput()
	t.Logf("jailed shell:\n%s", out)
	if err != nil {
		t.Fatalf("jailed shell: %v", err)
	}
	for _, ok := range []string{"write home", "move across home dirs", "truncate in home", "fifo in home", "write group", "write unit",
		"mktemp in TMPDIR", "python3", "git", "read /etc/passwd", "read /proc/self/status", "process substitution", "devices", "run a program"} {
		if abi < 2 && ok == "move across home dirs" {
			continue // ABI 1 refuses every rename across directories (mv copies instead)
		}
		if !strings.Contains(string(out), "jail: "+ok+": ok") {
			t.Errorf("%s: want ok", ok)
		}
	}
	for _, no := range []string{"read other layer", "write other layer", "list other layer", "write /tmp", "read /tmp", "write /var/tmp",
		"write /usr", "read /etc/shells", "read /etc/shadow", "list /", "list /root", "read the launcher", "list /opt"} {
		if !strings.Contains(string(out), "jail: "+no+": denied") {
			t.Errorf("%s: want denied", no)
		}
	}
	if _, err := os.Stat("/tmp/nt-jail-w"); err == nil {
		os.Remove("/tmp/nt-jail-w")
		t.Error("/tmp was written")
	}
}

// TestJailRefusesBadArgs: the launcher runs nothing it cannot jail.
func TestJailRefusesBadArgs(t *testing.T) {
	for _, args := range [][]string{
		{"--home", "/nonexistent-home", "--", "/bin/sh", "sh", "-c", "echo ran"},
		{"--", "/bin/sh", "sh", "-c", "echo ran"},
	} {
		if _, err := ABI(); err != nil && args[0] == "--home" {
			continue // no Landlock: the missing home is never reached
		}
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), launchEnv+"=1")
		out, err := cmd.CombinedOutput()
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != ExitJail || strings.Contains(string(out), "ran") {
			t.Errorf("%q: %v %s", args, err, out)
		}
	}
}

// This exercises the actual kernel restriction without containers or production
// resources. It runs on any Linux test host with Landlock ABI >=3.
func TestQuarantineReadOnlyJail(t *testing.T) {
	abi, err := ABI()
	if err != nil || abi < 3 {
		t.Skip("requires Landlock ABI 3 or newer")
	}
	home := t.TempDir()
	shared := t.TempDir()
	tmp := filepath.Join(home, TmpDir)
	for _, dir := range []string{tmp, filepath.Join(home, "scripts"), filepath.Join(home, "memories")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "memories", "note.txt"), []byte("note"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "read.txt"), []byte("shared"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "scripts", "read.sh"), []byte("#!/bin/sh\nprintf readable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "scripts"), filepath.Join(tmp, "escape")); err != nil {
		t.Fatal(err)
	}
	spec := Spec{Home: home, Shared: []string{shared}, ReadOnlyHome: true, Writable: []string{tmp}}
	self, _ := os.Executable()
	run := func(command string) ([]byte, error) {
		cmd := exec.Command(self, append(spec.Args()[1:], "/bin/sh", "sh", "-c", command)...)
		cmd.Dir = home
		cmd.Env = append(os.Environ(), launchEnv+"=1", "HOME="+home, "TMPDIR="+tmp)
		return cmd.CombinedOutput()
	}
	for _, command := range []string{"cat memories/note.txt", fmt.Sprintf("cat %q/read.txt", shared), "./scripts/read.sh", "printf scratch > \"$TMPDIR/scratch\"", "(printf background > \"$TMPDIR/bg\") & wait"} {
		if out, err := run(command); err != nil {
			t.Errorf("allowed command %q: %v %s", command, err, out)
		}
	}
	for _, command := range []string{"printf tainted > scripts/new.sh", "printf tainted > memories/note.txt", fmt.Sprintf("printf tainted > %q/new.txt", shared), "printf tainted > .tmp/escape/new.sh", "truncate -s 0 memories/note.txt", "(printf tainted > scripts/bg.sh) & wait $!"} {
		if out, err := run(command); err == nil {
			t.Errorf("persistent write allowed %q: %s", command, out)
		}
	}
	if note, err := os.ReadFile(filepath.Join(home, "memories/note.txt")); err != nil || string(note) != "note" {
		t.Fatal("readonly memory changed")
	}
	// Replacing the writable root itself must not grant write access to its target.
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, tmp); err != nil {
		t.Fatal(err)
	}
	if out, err := run("printf should-not-run"); err == nil || strings.Contains(string(out), "should-not-run") {
		t.Fatalf("temp root symlink was followed: %v %s", err, out)
	}
}
