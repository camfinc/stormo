package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/internal/needletail/jail"
	"github.com/camfinc/stormo/internal/needletail/messaging"
	"github.com/camfinc/stormo/internal/needletail/wire"
)

// Item 8 (Claude M4): an approval lifts the gate, never the jail: a risky command a person
// approved in a tainted conversation still runs as a tainted command (read-only jail, its work
// directory, no network, no credentials), and without such a jail it does not run at all.
func TestApprovedCommandKeepsTheTaintedJail(t *testing.T) {
	r, env := setup(t)
	env.Session = "s1"
	env.Untrusted = func(ctx context.Context, _ string) string {
		if messaging.Endorsed(ctx) != "" {
			return "" // as the runtime's: an approved action is endorsed
		}
		return "this conversation has taken in words you may not obey"
	}
	ctx := messaging.WithEndorsed(context.Background(), "ap_1")
	args, _ := json.Marshal(map[string]any{"command": "echo ran > ran.txt"})
	out, failed := r.Call(ctx, env, "terminal", string(args))
	if !failed || !strings.Contains(out, "jail") {
		t.Errorf("an approved command from a tainted conversation without a tainted jail: %s", out)
	}
	if _, err := os.Stat(filepath.Join(env.Home, "ran.txt")); err == nil {
		t.Fatal("it ran unjailed")
	}
	// with a launcher: the tainted spec (read-only home, its work directory)
	env.Jail = &jail.Launcher{Self: "/nonexistent", Spec: jail.Spec{Home: env.Home}}
	tainted, err := commandEnv(ctx, env)
	if err == nil && (!tainted.tainted || !tainted.Jail.Spec.ReadOnlyHome) {
		t.Fatalf("an approved command's env is not the tainted one: %+v", tainted.Jail.Spec)
	}
}

// Item 12 (Codex 1): a tainted command is started in the work area's group (its own work
// directory reachable, the work area's root not listable), a trusted one never: the work area
// (0710, its group's) keeps every trusted command out of every conversation's work directory.
func TestCommandGroups(t *testing.T) {
	agent := &User{UID: 10001, GID: 10000}
	trusted := &Env{Agent: agent}
	tainted := &Env{Agent: agent, tainted: true}
	for _, c := range []struct {
		env  *Env
		want []uint32
	}{{trusted, []uint32{}}, {tainted, []uint32{wire.WorkGID}}} {
		cmd := newCmd(c.env)
		if got := cmd.SysProcAttr.Credential.Groups; !slices.Equal(got, c.want) {
			t.Errorf("tainted %v: groups %v, want %v", c.env.tainted, got, c.want)
		}
	}
}

func newCmd(env *Env) *exec.Cmd {
	cmd := exec.Command("true")
	command(cmd, env)
	return cmd
}
