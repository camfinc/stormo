package jail

import (
	"os/exec"
	"slices"
	"strings"
	"testing"
)

func TestHandledPerABI(t *testing.T) {
	base := uint64(0x1fff) // ABI 1: execute .. make_sym
	for _, c := range []struct {
		abi  int
		want uint64
	}{
		{0, 0}, {-1, 0},
		{1, base},
		{2, base | Refer},
		{3, base | Refer | Truncate},
		{4, base | Refer | Truncate}, // ABI 4 adds network rights only
		{5, base | Refer | Truncate | IoctlDev},
		{10, base | Refer | Truncate | IoctlDev},
	} {
		if got := Handled(c.abi); got != c.want {
			t.Errorf("Handled(%d) = %#x, want %#x", c.abi, got, c.want)
		}
	}
}

func TestGrantMasksPerABIAndFileType(t *testing.T) {
	home := Rule{Path: "/opt/data", Access: RW}
	if g := home.Grant(1, true); g&(Refer|Truncate|IoctlDev) != 0 || g&(WriteFile|MakeReg|RemoveFile) == 0 {
		t.Errorf("ABI 1 home grant %#x: no refer, truncate or ioctl; write, make and remove", g)
	}
	if g := home.Grant(3, true); g&Refer == 0 || g&Truncate == 0 || g&IoctlDev != 0 {
		t.Errorf("ABI 3 home grant %#x: refer and truncate, no ioctl", g)
	}
	if g := home.Grant(5, true); g != RW {
		t.Errorf("ABI 5 home grant %#x, want RW %#x", g, RW)
	}
	if RW&(MakeChar|MakeBlock) != 0 {
		t.Error("the agent's directories never allow device nodes")
	}
	// a rule on a file keeps only a file's rights (a directory's would be EINVAL)
	if g := (Rule{Path: "/etc/passwd", Access: RO}).Grant(5, false); g != ReadFile {
		t.Errorf("/etc/passwd grant %#x, want read file only", g)
	}
	if g := (Rule{Path: "/dev/null", Access: Dev}).Grant(1, false); g != ReadFile|WriteFile {
		t.Errorf("ABI 1 /dev/null grant %#x, want read and write", g)
	}
	if g := (Rule{Path: "/dev/null", Access: Dev}).Grant(5, false); g != ReadFile|WriteFile|Truncate|IoctlDev {
		t.Errorf("ABI 5 /dev/null grant %#x", g)
	}
	if g := (Rule{Path: "/usr/bin", Access: RX}).Grant(5, true); g != RX {
		t.Errorf("/usr/bin grants %#x, want read and execute", g)
	}
	if g := (Rule{Path: "/x", Access: RW}).Grant(0, true); g != 0 {
		t.Errorf("no Landlock grants %#x", g)
	}
}

func TestRules(t *testing.T) {
	rules := Spec{Home: "/opt/data", Shared: []string{"/shared/group", "/shared/sales"}}.Rules()
	by := map[string]Rule{}
	for _, r := range rules {
		if _, dup := by[r.Path]; dup {
			t.Errorf("%s twice", r.Path)
		}
		by[r.Path] = r
	}
	if r := by["/opt/data"]; r.Access != RW || !r.Required {
		t.Errorf("home rule %+v", r)
	}
	for _, s := range []string{"/shared/group", "/shared/sales"} {
		if r := by[s]; r.Access != RW || r.Required {
			t.Errorf("%s rule %+v: read-write, skipped when not mounted", s, r)
		}
	}
	for _, p := range []string{"/bin", "/lib", "/usr/bin", "/usr/lib"} {
		if by[p].Access != RX {
			t.Errorf("%s: read and execute, got %#x", p, by[p].Access)
		}
	}
	for _, p := range []string{"/etc/ssl", "/etc/passwd", "/etc/resolv.conf", "/etc/hosts", "/proc"} {
		if by[p].Access != RO {
			t.Errorf("%s: read only, got %#x", p, by[p].Access)
		}
	}
	// nothing outside the agent's directories is writable but the devices; nothing else of
	// the image is reachable at all
	for _, r := range rules {
		writes := r.Access&(WriteFile|MakeReg|MakeDir|RemoveFile|RemoveDir|Truncate) != 0
		switch {
		case r.Path == "/opt/data" || strings.HasPrefix(r.Path, "/shared/"):
		case strings.HasPrefix(r.Path, "/dev/"):
			if r.Access != Dev {
				t.Errorf("%s: %#x", r.Path, r.Access)
			}
		case writes:
			t.Errorf("%s is writable", r.Path)
		}
		for _, denied := range []string{"/", "/tmp", "/var", "/var/tmp", "/root", "/etc", "/usr", "/usr/local", "/opt", "/shared", "/app", "/etc/shadow", "/home", "/run", "/dev", "/dev/shm", "/sys"} {
			if r.Path == denied {
				t.Errorf("%s has a rule", denied)
			}
		}
	}
	if by["/usr/local/bin"].Access != RX {
		t.Error("execve needs read and execute: /usr/local/bin's programs must be both")
	}
}

func TestWrapAndParse(t *testing.T) {
	l := &Launcher{Self: "/usr/local/bin/needletail", Spec: Spec{Home: "/opt/data", Shared: []string{"/shared/group", "/shared/sales"}}}
	cmd := exec.Command("/bin/bash", "--noprofile", "--norc", "-c", "echo -- --home x")
	cmd.Args[0] = "bash"
	l.Wrap(cmd)
	if cmd.Path != l.Self {
		t.Fatalf("path %s", cmd.Path)
	}
	want := []string{"needletail", "jail", "--home", "/opt/data", "--shared", "/shared/group", "--shared", "/shared/sales", "--",
		"/bin/bash", "bash", "--noprofile", "--norc", "-c", "echo -- --home x"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("args %q", cmd.Args)
	}
	spec, path, argv, err := parse(cmd.Args[2:])
	if err != nil {
		t.Fatal(err)
	}
	if spec.Home != "/opt/data" || !slices.Equal(spec.Shared, l.Spec.Shared) || path != "/bin/bash" || !slices.Equal(argv, want[10:]) {
		t.Fatalf("parsed %+v %s %q", spec, path, argv)
	}
	for _, bad := range [][]string{
		{"--home", "/opt/data", "/bin/bash", "bash"},           // no --
		{"--home", "rel", "--", "/bin/bash", "bash"},           // relative home
		{"--", "/bin/bash", "bash"},                            // no home
		{"--home", "/h", "--", "bash", "bash"},                 // relative command
		{"--home", "/h", "--", "/bin/bash"},                    // no argv
		{"--home", "/h", "--shared", "s", "--", "/bin/x", "x"}, // relative layer
		{"--home", "/h", "--bogus", "--", "/bin/x", "x"},       // unknown flag
	} {
		if _, _, _, err := parse(bad); err == nil {
			t.Errorf("parse(%q) accepted", bad)
		}
	}
}

func TestQuarantineLauncherFlagsRequired(t *testing.T) {
	_, _, _, err := parse([]string{"--home", "/opt/data", "--read-only-home", "--writable", "/opt/data/.tmp", "--", "/bin/bash", "bash"})
	if err != nil {
		t.Fatalf("read-only quarantine jail flags refused: %v", err)
	}
}

func TestQuarantineRulesAndWritableValidation(t *testing.T) {
	spec := Spec{Home: "/opt/data", Shared: []string{"/shared/group"}, ReadOnlyHome: true, Writable: []string{"/opt/data/.tmp"}}
	rules := map[string]Rule{}
	for _, r := range spec.Rules() {
		rules[r.Path] = r
	}
	if rules[spec.Home].Access != RX || rules["/shared/group"].Access != RO {
		t.Fatal("home/shared are writable in quarantine")
	}
	tmp := rules["/opt/data/.tmp"]
	if tmp.Access != RW || !tmp.Required || !tmp.NoFollow {
		t.Fatalf("unsafe temp grant %+v", tmp)
	}
	parsed, _, _, err := parse(append(spec.Args()[1:], "/bin/sh", "sh"))
	if err != nil || !parsed.ReadOnlyHome || len(parsed.Writable) != 1 {
		t.Fatalf("quarantine launcher roundtrip %+v %v", parsed, err)
	}
	for _, path := range []string{"/opt/data", "/shared/group", "/opt/data/.tmp/child", "/opt/data/.tmp/../scripts", "/opt/data/work", "/opt/data/work/a/b",
		"/opt/data/work/..", "/opt/data/work/../scripts", "/opt/data/work/", "work/conv", "/opt/data/skills/conv"} {
		_, _, _, err := parse([]string{"--home", "/opt/data", "--read-only-home", "--writable", path, "--", "/bin/sh", "sh"})
		if err == nil {
			t.Errorf("a writable grant outside a conversation's work directory accepted: %s", path)
		}
	}
	// a tainted run's grant: its conversation's work directory
	for _, path := range []string{"/opt/data/work/slack-C0DEVCHAN1-1700000100.000100", "/opt/data/work/conv"} {
		if _, _, _, err := parse([]string{"--home", "/opt/data", "--read-only-home", "--writable", path, "--", "/bin/sh", "sh"}); err != nil {
			t.Errorf("a work directory grant refused: %s: %v", path, err)
		}
	}
	if _, _, _, err := parse([]string{"--home", "/opt/data", "--read-only-home", "--writable", "/opt/data/work/a", "--writable", "/opt/data/work/b", "--", "/bin/sh", "sh"}); err == nil {
		t.Error("two writable grants accepted")
	}
}
