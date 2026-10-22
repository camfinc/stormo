// Package privsep splits the runtime from what the agent executes (docs/needletail.md §7): the
// container starts as root; the runtime lays the home out (Prepare) so that its own state is
// its user's alone, then drops to that user keeping only the capabilities it needs to start the
// agent's commands as a second user and to stop them (Drop). The terminal, scheduled scripts
// and monitors then run as ExecUID with the runtime's group and no capabilities: they can work
// in the agent's workspace and the shared space, never in cron/, state/, sessions/, logs/,
// .swarm/, memories/ (read-only) or needletail.yaml, nor read the runtime's /proc.
package privsep

import (
	"fmt"
	"os"
	"runtime"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Users are the two users of the split.
type Users struct {
	RuntimeUID, RuntimeGID, ExecUID int
}

// Default is the image's: wire.RuntimeUID (the shared space's owner) and wire.ExecUID.
var Default = Users{RuntimeUID: wire.RuntimeUID, RuntimeGID: wire.RuntimeGID, ExecUID: wire.ExecUID}

// The home's layout, relative to the home. Every other entry is the agent's workspace (skills/,
// scripts/, the manifest's state dir, what its commands make): the exec user's, group-writable,
// so the runtime (same group) writes there too.
var (
	// Private are the runtime's own directories: its user's, 0700 (files 0600), made when
	// missing so a command cannot take the name first. Parents come before children.
	Private = []string{"cron", "state", "state/needletail", "sessions", "logs", "logs/audit", ".swarm", fsx.TmpDir}
	// ReadOnly are the runtime's directories the agent's commands may read (group r-x): the
	// memory, which is in the system prompt anyway and changes only through the memory tool.
	ReadOnly = []string{"memories"}
	// Files are the runtime's files at the home's root, with their mode: made empty when
	// missing (SOUL.md; a missing needletail.yaml fails the boot earlier).
	Files = map[string]uint32{"needletail.yaml": 0o600, "SOUL.md": 0o644}
)

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

// Enter splits the runtime when it starts as root on Linux (the image): Prepare, then Drop, then
// the umask that keeps what both users make group-writable. Started as anyone else it runs as
// one user, as before (logged). A failure of a split that should happen is an error: the
// runtime does not run with the agent's commands able to reach its state.
func Enter(home string, logf func(string)) (Split, error) {
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
	if err := Prepare(home, u, logf); err != nil {
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
