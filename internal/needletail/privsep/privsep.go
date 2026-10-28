// Package privsep splits the runtime from what the agent executes (docs/needletail.md §7): the
// container starts as root; the runtime lays the home out (Prepare) so that its own state is
// its user's alone, then drops to that user keeping only the capabilities it needs to start the
// agent's commands as a second user and to stop them (Drop). The terminal, scheduled scripts
// and monitors then run as ExecUID with the runtime's group and no capabilities: they can work
// in the agent's workspace and the shared space, never in cron/, state/, sessions/, logs/,
// .swarm/, memories/ (the hot files read-only, the archive not even that) or needletail.yaml, nor
// read the runtime's /proc.
package privsep

import (
	"fmt"
	"os"
	"runtime"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/jail"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Users are the two users of the split.
type Users struct {
	RuntimeUID, RuntimeGID, ExecUID int
	// WorkGID is the work area's group (wire.WorkGID; 0: the runtime's group, as a test without
	// root has it).
	WorkGID int
}

// Default is the image's: wire.RuntimeUID (the shared space's owner) and wire.ExecUID.
var Default = Users{RuntimeUID: wire.RuntimeUID, RuntimeGID: wire.RuntimeGID, ExecUID: wire.ExecUID, WorkGID: wire.WorkGID}

// The home's layout, relative to the home. Every other entry is the agent's workspace (skills/,
// scripts/, the manifest's state dir, what its commands make): the exec user's, group-writable,
// so the runtime (same group) writes there too.
var (
	// Private are the runtime's own directories: its user's, 0700 (files 0600), made when
	// missing so a command cannot take the name first. Parents come before children. files/ is
	// the store of people's uploads (client data, every conversation's): no command reads it.
	Private = []string{"cron", "state", "state/needletail", "sessions", "logs", "logs/audit", ".swarm", fsx.TmpDir, jail.FileStore}
	// ReadOnly are the runtime's directories the agent's commands may read (group r-x): the
	// memory, which is in the system prompt anyway and changes only through the memory tool.
	ReadOnly = []string{"memories"}
	// PrivateFiles are the runtime's files inside a ReadOnly directory that stay its user's alone
	// (0600): the memory archive, every memory with client data in it, which the commands never
	// read (the memory tools search it within a turn's audience). A missing one is not made.
	PrivateFiles = []string{"memories/RECORDS.md"}
	// Files are the runtime's files at the home's root, with their mode: made empty when
	// missing (SOUL.md; a missing needletail.yaml fails the boot earlier).
	Files = map[string]uint32{"needletail.yaml": 0o600, "SOUL.md": 0o644}
	// Workspace are the workspace's trees the runtime reads and writes for the agent (with the
	// manifest's state dir, needletail.yaml's state_root): the group may reach every entry in them,
	// one a command made owner-only included.
	Workspace = []string{"skills", "scripts"}
	// Temp is the commands' temporary directory (TMPDIR, jail.TmpDir): the runtime's, so a
	// command can neither remove nor replace it (a symlink to /tmp, say; the home is sticky), and
	// group-writable (TempMode) for the commands; what they make in it is theirs, as in the rest
	// of the workspace.
	Temp = jail.TmpDir
	// Work is the work area (tools.WorkRoot): work/<conversation>/ is where a tainted run's
	// writes go. The runtime's, in the work group (Users.WorkGID), WorkMode: no command lists it
	// or makes anything in it (so none renames, removes or swaps a conversation's directory),
	// and only a command started in the work group (a tainted run's, jailed to its own
	// conversation's directory) enters it: a trusted command reaches no conversation's work.
	Work = "work"
)

// WorkMode is Work's own mode: the runtime's, entered by its group.
const WorkMode = 0o710

// TempMode is Temp's own mode: the runtime's group may make entries there (setgid: they keep it).
const TempMode = 0o2770

// RootMode is the home's own mode: the runtime's, group-writable so the agent's commands can make
// files and directories there, sticky so they can never remove or rename the runtime's entries
// (cron/, needletail.yaml, …), setgid so what they make keeps the runtime's group.
const RootMode = 0o3775

// Split is the runtime's state after Enter: Exec is who the agent's commands run as (nil: as the
// runtime itself, the split being off).
type Split struct {
	Exec *Users
	// Why the split is off, when it is.
	Off string
}

// OffEnv turns the split off for a runtime started as root ("off"), for development and tests
// only: the agent's commands then run as root. Never set in the image.
const OffEnv = "NEEDLETAIL_UID_SPLIT"

// Enter splits the runtime when it starts as root on Linux (the image): Prepare (stateDir is the
// manifest's state dir, relative to the home), then Drop, then the umask that keeps what both
// users make group-writable. Started as anyone else it runs as
// one user, as before (logged). A failure of a split that should happen is an error: the
// runtime does not run with the agent's commands able to reach its state.
func Enter(home, stateDir string, logf func(string)) (Split, error) {
	switch {
	case runtime.GOOS != "linux":
		return Split{Off: "not Linux"}, nil
	case os.Geteuid() != 0:
		s := Split{Off: fmt.Sprintf("started as uid %d, not root", os.Geteuid())}
		logf("uid split off: " + s.Off + ": the agent's commands run as the runtime's user and can reach its files")
		return s, nil
	case os.Getenv(OffEnv) == "off":
		s := Split{Off: OffEnv + "=off"}
		logf("uid split off (" + s.Off + "): running as root, development only")
		return s, nil
	}
	u := Default
	if err := Prepare(home, u, stateDir, logf); err != nil {
		return Split{}, fmt.Errorf("uid split: preparing the home: %w", err)
	}
	if err := Drop(u); err != nil {
		return Split{}, fmt.Errorf("uid split: dropping to uid %d: %w", u.RuntimeUID, err)
	}
	umask(0o002)
	if bnd, ok := Bounding(); !ok {
		logf(fmt.Sprintf("uid split: the bounding set could not be narrowed (no CAP_SETPCAP): it is %016x; the agent's commands still start with no capability", bnd))
	}
	logf(fmt.Sprintf("uid split: the runtime runs as %d:%d, the agent's commands as %d:%d with no capabilities", u.RuntimeUID, u.RuntimeGID, u.ExecUID, u.RuntimeGID))
	return Split{Exec: &u}, nil
}
