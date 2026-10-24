//go:build linux

package privsep

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDropAsRoot is the split for real: as root (a container: `go test -c` for linux, then run
// the binary as root with the compose hardening flags; docs/needletail.md §7), a child process
// prepares a home and drops to the runtime's user, then starts a command as the exec user the
// way the terminal does and reports what it could do. Skipped when not root.
func TestDropAsRoot(t *testing.T) {
	if os.Getenv("NT_DROP_CHILD") == "1" {
		dropChild()
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root: run the test binary in a container")
	}
	home := t.TempDir()
	if err := os.Chmod(filepath.Dir(home), 0o755); err != nil { // the test's own temp root is 0700
		t.Fatal(err)
	}
	for rel, body := range map[string]string{"cron/jobs.json": `{"jobs":[]}`, "state/needletail/idem.json": "{}", "needletail.yaml": "version: 1\n",
		".swarm/booted.json": "{}", "memories/MEMORY.md": "a note",
		"memories/RECORDS.md": "<!-- needletail-records v1 -->\n§\nan archived client fact", "state/needletail/memory-usage.json": "{}", "skills/demo/SKILL.md": "---\nname: demo\n---\n", "SOUL.md": "me"} {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestDropAsRoot$")
	cmd.Env = append(os.Environ(), "NT_DROP_CHILD=1", "NT_HOME="+home, "NT_SECRET=s3cr3t-in-the-runtime-env")
	out, err := cmd.CombinedOutput()
	t.Logf("child:\n%s", out)
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	for _, want := range []string{
		"runtime: verified",
		"exec: uid=10001 gid=10000 groups=",
		"CapPrm=0000000000000000 CapEff=0000000000000000",
		"CapBnd=00000000000000e0", // the bounding set kept only KILL, SETGID, SETUID
		"exec: write cron/jobs.json: denied",
		"exec: write state/needletail/idem.json: denied",
		"exec: write needletail.yaml: denied",
		"exec: write .swarm/booted.json: denied",
		"exec: write memories/MEMORY.md: denied",
		"exec: read memories/MEMORY.md: ok",
		"exec: read memories/RECORDS.md: denied",
		"exec: read state/needletail/memory-usage.json: denied",
		"exec: list memories/: ok",
		"exec: replace cron: denied",
		"exec: read /proc/runtime/environ: denied",
		"exec: signal the runtime: denied",
		"exec: write skills/demo/scripts/run.sh: ok",
		"exec: make work/: ok",
		"runtime: wrote skills/demo/scripts/run.sh: ok",
		"runtime: killed the command's group: ok",
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(string(out), "s3cr3t-in-the-runtime-env") {
		t.Error("the runtime's start-up env reached the exec user")
	}
}

// dropChild is the runtime: Prepare, Drop, then a command as the exec user.
func dropChild() {
	home := os.Getenv("NT_HOME")
	fail := func(f string, a ...any) { fmt.Printf("FAIL "+f+"\n", a...); os.Exit(1) }
	if err := Prepare(home, Default, "acme-state", func(s string) { fmt.Println("log:", s) }); err != nil {
		fail("prepare: %v", err)
	}
	if err := Drop(Default); err != nil {
		fail("drop: %v", err)
	}
	umask(0o002)
	if err := Verify(Default); err != nil {
		fail("verify: %v", err)
	}
	fmt.Println("runtime: verified")
	pid := os.Getpid()
	script := fmt.Sprintf(`cd %q
echo "exec: uid=$(id -u) gid=$(id -g) groups=$(id -G | tr ' ' '\n' | grep -v '^10000$' | tr '\n' ' ')"
echo "exec: $(grep -E '^Cap(Prm|Eff|Bnd|Amb)' /proc/self/status | tr -d '\t' | tr '\n' ' ' | sed 's/:/=/g')"
try() { if eval "$2" 2>/dev/null; then echo "exec: $1: ok"; else echo "exec: $1: denied"; fi; }
try "write cron/jobs.json" 'echo x > cron/jobs.json'
try "write state/needletail/idem.json" 'echo x > state/needletail/idem.json'
try "write needletail.yaml" 'echo x >> needletail.yaml'
try "write .swarm/booted.json" 'echo x > .swarm/booted.json'
try "write memories/MEMORY.md" 'echo x >> memories/MEMORY.md'
try "read memories/MEMORY.md" 'cat memories/MEMORY.md >/dev/null'
try "read memories/RECORDS.md" 'cat memories/RECORDS.md >/dev/null'
try "read state/needletail/memory-usage.json" 'cat state/needletail/memory-usage.json >/dev/null'
try "list memories/" 'ls memories >/dev/null'
try "replace cron" 'mv cron cron.old'
try "read /proc/runtime/environ" 'cat /proc/%d/environ >/dev/null'
try "signal the runtime" 'kill -0 %d'
try "write skills/demo/scripts/run.sh" 'mkdir -p skills/demo/scripts && echo "echo hi" > skills/demo/scripts/run.sh'
try "make work/" 'mkdir work && echo x > work/f'
sleep 30 &
wait
`, home, pid, pid)
	cmd := exec.Command("sh", "-c", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Credential: &syscall.Credential{Uid: uint32(Default.ExecUID), Gid: uint32(Default.RuntimeGID), Groups: []uint32{}}}
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + home}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stdout
	if err := cmd.Start(); err != nil {
		fail("start as the exec user: %v", err)
	}
	time.Sleep(2 * time.Second) // the probes ran; the command waits on its background sleep
	if f, err := os.OpenFile(filepath.Join(home, "skills/demo/scripts/run.sh"), os.O_WRONLY|os.O_APPEND, 0); err == nil {
		_, err = f.WriteString("echo more\n")
		f.Close()
		fmt.Println("runtime: wrote skills/demo/scripts/run.sh:", map[bool]string{true: "ok", false: "failed " + fmt.Sprint(err)}[err == nil])
	} else {
		fmt.Println("runtime: wrote skills/demo/scripts/run.sh: failed", err)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		fail("kill the command's group: %v", err)
	}
	_ = cmd.Wait()
	fmt.Println("runtime: killed the command's group: ok")
	st, err := os.Stat(filepath.Join(home, "cron"))
	if err != nil || !st.IsDir() {
		fail("cron is gone")
	}
	fmt.Println("runtime: uid", strconv.Itoa(os.Getuid()))
}
