// Package jail confines what the agent executes (the terminal, scheduled scripts and monitors) to
// the directories mounted for it, with Landlock (docs/needletail.md §7): read-write in its home
// and its shared layers (/shared/group, /shared/<unit>), read and execute on the system's
// programs and libraries, read on the few /etc files a tool needs to resolve names, users and
// TLS roots, read on /proc, the harmless devices, and nothing else: no /tmp, no /root, no other
// unit's layer, no other file of the image.
//
// The runtime cannot run code in a child between fork and exec, so a command starts through the
// runtime's own binary (Launcher.Wrap): `needletail jail --home H --shared S… -- <path> <argv…>`
// restricts itself (Main) and then execs the command, which inherits the restriction, as does
// everything it starts. Landlock only takes rights away: the uid split's file modes still keep
// the runtime's own directories out of the commands' reach inside the home.
package jail

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
)

// Landlock's filesystem access rights (include/uapi/linux/landlock.h; the kernel ABI, stable).
const (
	Execute    uint64 = 1 << 0
	WriteFile  uint64 = 1 << 1
	ReadFile   uint64 = 1 << 2
	ReadDir    uint64 = 1 << 3
	RemoveDir  uint64 = 1 << 4
	RemoveFile uint64 = 1 << 5
	MakeChar   uint64 = 1 << 6
	MakeDir    uint64 = 1 << 7
	MakeReg    uint64 = 1 << 8
	MakeSock   uint64 = 1 << 9
	MakeFifo   uint64 = 1 << 10
	MakeBlock  uint64 = 1 << 11
	MakeSym    uint64 = 1 << 12
	// Refer (ABI 2) is renaming or linking a file into another directory. Without it (ABI 1) the
	// kernel refuses every such move (EXDEV; mv then copies and removes).
	Refer uint64 = 1 << 13
	// Truncate (ABI 3) is truncate(2) and open with O_TRUNC (`> file`).
	Truncate uint64 = 1 << 14
	// IoctlDev (ABI 5) is ioctl(2) on a device file.
	IoctlDev uint64 = 1 << 15
)

// The rights a rule grants.
const (
	// FileRights are the rights that apply to a file (a rule on a file may hold no other).
	FileRights = Execute | WriteFile | ReadFile | Truncate | IoctlDev
	// RW is the agent's own directories: everything but making device nodes.
	RW = Execute | WriteFile | ReadFile | ReadDir | RemoveDir | RemoveFile | MakeDir | MakeReg | MakeSock | MakeFifo | MakeSym | Refer | Truncate | IoctlDev
	// RX is the system's programs and libraries.
	RX = Execute | ReadFile | ReadDir
	// RO is read only: /etc's files, /proc.
	RO = ReadFile | ReadDir
	// Dev is a harmless device: /dev/null and the like.
	Dev = ReadFile | WriteFile | Truncate | IoctlDev
)

// Handled is every right the kernel's Landlock ABI knows: the ruleset handles (denies unless a
// rule grants) all of them. 0 for no Landlock. Rights of later ABIs than this package knows are
// not handled (best effort: the kernel then does not restrict them); network and scopes are not
// handled at all (the jail is about the filesystem).
func Handled(abi int) uint64 {
	if abi < 1 {
		return 0
	}
	h := Execute | WriteFile | ReadFile | ReadDir | RemoveDir | RemoveFile | MakeChar | MakeDir | MakeReg | MakeSock | MakeFifo | MakeBlock | MakeSym
	if abi >= 2 {
		h |= Refer
	}
	if abi >= 3 {
		h |= Truncate
	}
	if abi >= 5 {
		h |= IoctlDev
	}
	return h
}

// Rule grants Access beneath Path (a directory and everything below it, or one file).
type Rule struct {
	Path   string
	Access uint64
	// Required: the jail fails when Path is missing (the home); other paths are skipped, so one
	// list serves images that lack some of them.
	Required bool
	// NoFollow pins a writable directory itself, never a symlink target.
	NoFollow bool
}

// Grant is the rule's rights as the kernel takes them: those the ABI handles, and only a file's
// rights on a file (any other is EINVAL there). 0: nothing to add.
func (r Rule) Grant(abi int, dir bool) uint64 {
	a := r.Access & Handled(abi)
	if !dir {
		a &= FileRights
	}
	return a
}

// TmpDir is the commands' temporary directory, below the home: TMPDIR, TMP and TEMP point there,
// since /tmp is outside the jail (and, with a read-only root filesystem, not writable at all). No
// snapshot rule claims it, so a nap never keeps it.
const TmpDir = ".tmp"

// System are the rules every jail has besides the home and the shared layers: the system's
// programs and libraries read and execute (Landlock cannot grant execute without read: execve
// opens the file for reading, so /usr/local/bin, which holds the runtime's binary, is readable
// too; the binary holds no secret), the files of /etc a command needs (names, users, DNS,
// TLS roots, git's and python's system files), /proc read only (a rule on /proc/self would bind
// the launcher's own pid, not its children's; the runtime's entries there stay root's, since it
// is non-dumpable and another uid) and the harmless devices.
var System = []Rule{
	{Path: "/bin", Access: RX},
	{Path: "/sbin", Access: RX},
	{Path: "/lib", Access: RX},
	{Path: "/lib64", Access: RX},
	{Path: "/usr/bin", Access: RX},
	{Path: "/usr/sbin", Access: RX},
	{Path: "/usr/lib", Access: RX},
	{Path: "/usr/lib64", Access: RX},
	{Path: "/usr/libexec", Access: RX},
	{Path: "/usr/share", Access: RX},
	{Path: "/usr/local/bin", Access: RX},
	{Path: "/usr/local/sbin", Access: RX},
	{Path: "/usr/local/lib", Access: RX},
	{Path: "/usr/local/libexec", Access: RX},
	{Path: "/usr/local/share", Access: RX},
	{Path: "/etc/ssl", Access: RO},
	{Path: "/etc/ca-certificates", Access: RO},
	{Path: "/etc/passwd", Access: RO},
	{Path: "/etc/group", Access: RO},
	{Path: "/etc/hosts", Access: RO},
	{Path: "/etc/resolv.conf", Access: RO},
	{Path: "/etc/nsswitch.conf", Access: RO},
	{Path: "/etc/host.conf", Access: RO},
	{Path: "/etc/services", Access: RO},
	{Path: "/etc/protocols", Access: RO},
	{Path: "/etc/hostname", Access: RO},
	{Path: "/etc/localtime", Access: RO},
	{Path: "/etc/os-release", Access: RO},
	{Path: "/etc/alpine-release", Access: RO},
	{Path: "/etc/mime.types", Access: RO},
	{Path: "/etc/gitconfig", Access: RO},
	{Path: "/etc/ld.so.cache", Access: RO},
	{Path: "/etc/ld-musl-aarch64.path", Access: RO},
	{Path: "/etc/ld-musl-x86_64.path", Access: RO},
	{Path: "/proc", Access: RO},
	{Path: "/dev/null", Access: Dev},
	{Path: "/dev/zero", Access: Dev},
	{Path: "/dev/full", Access: Dev},
	{Path: "/dev/random", Access: Dev},
	{Path: "/dev/urandom", Access: Dev},
	{Path: "/dev/tty", Access: Dev},
}

// Spec is one agent's jail: its home and its shared layers.
type Spec struct {
	Home         string
	Shared       []string
	ReadOnlyHome bool
	Writable     []string
}

// Rules are the spec's rules: the home (required), each shared layer and System.
func (s Spec) Rules() []Rule {
	access, shared := uint64(RW), uint64(RW)
	if s.ReadOnlyHome {
		access, shared = RX, RO
	}
	out := []Rule{{Path: s.Home, Access: access, Required: true}}
	for _, d := range s.Shared {
		out = append(out, Rule{Path: d, Access: shared})
	}
	if s.ReadOnlyHome {
		for _, d := range s.Writable {
			out = append(out, Rule{Path: d, Access: RW, Required: true, NoFollow: true})
		}
	}
	return append(out, System...)
}

// Args are the launcher's arguments before the command: `jail --home H --shared S… --`.
func (s Spec) Args() []string {
	out := []string{"jail", "--home", s.Home}
	for _, d := range s.Shared {
		out = append(out, "--shared", d)
	}
	if s.ReadOnlyHome {
		out = append(out, "--read-only-home")
	}
	for _, d := range s.Writable {
		out = append(out, "--writable", d)
	}
	return append(out, "--")
}

// Launcher starts commands jailed: Self is the runtime's binary.
type Launcher struct {
	Self string
	Spec Spec
}

// Wrap makes cmd (path resolved, args, env and credentials set) start through the launcher: the
// launcher runs with cmd's user, directory and process group, restricts itself, then execs
// cmd's program with cmd's arguments (argv[0] included) and environment.
func (l *Launcher) Wrap(cmd *exec.Cmd) {
	args := append([]string{"needletail"}, l.Spec.Args()...)
	args = append(args, cmd.Path)
	cmd.Args = append(args, cmd.Args...)
	cmd.Path = l.Self
}

// Exit codes of the launcher itself (the command's own are passed through by exec).
const (
	// ExitJail: the jail could not be set up; the command did not run.
	ExitJail = 125
	// ExitExec: the command could not be executed (not found, or not executable in the jail).
	ExitExec = 126
)

// parse reads the launcher's arguments: the spec, the command's path and its argv.
func parse(args []string) (Spec, string, []string, error) {
	fs := flag.NewFlagSet("jail", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var s Spec
	fs.StringVar(&s.Home, "home", "", "")
	fs.BoolVar(&s.ReadOnlyHome, "read-only-home", false, "")
	fs.Func("writable", "", func(v string) error { s.Writable = append(s.Writable, v); return nil })
	fs.Func("shared", "", func(v string) error { s.Shared = append(s.Shared, v); return nil })
	if err := fs.Parse(args); err != nil {
		return Spec{}, "", nil, err
	}
	rest := fs.Args()
	// flag stops at "--" and drops it; a spec with no "--" would take the command's flags
	if len(args) < len(rest)+1 || args[len(args)-len(rest)-1] != "--" {
		return Spec{}, "", nil, errors.New("missing -- before the command")
	}
	if s.Home == "" || !strings.HasPrefix(s.Home, "/") {
		return Spec{}, "", nil, errors.New("--home must be an absolute path")
	}
	if s.ReadOnlyHome {
		if len(s.Writable) != 1 || filepath.Clean(s.Writable[0]) != filepath.Join(filepath.Clean(s.Home), TmpDir) {
			return Spec{}, "", nil, errors.New("read-only home requires only its .tmp directory writable")
		}
	} else if len(s.Writable) != 0 {
		return Spec{}, "", nil, errors.New("writable overrides require read-only home")
	}
	for _, d := range s.Shared {
		if !strings.HasPrefix(d, "/") {
			return Spec{}, "", nil, fmt.Errorf("--shared %q is not an absolute path", d)
		}
	}
	if len(rest) < 2 || !strings.HasPrefix(rest[0], "/") {
		return Spec{}, "", nil, errors.New("the command must be an absolute path and its argv")
	}
	return s, rest[0], rest[1:], nil
}
