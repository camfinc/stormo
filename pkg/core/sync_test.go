package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/engine/hermes"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/link"
)

func TestRunnerSyncKeepsAHostsCopyInStep(t *testing.T) {
	_, inst := fixture(t)
	inst.Placements = []instance.Placement{{Name: "local", Kind: instance.PlacementLocal}, {Name: "remote", Kind: instance.PlacementECS}, {Name: "vps", Kind: instance.PlacementRunner}}
	coreDir, hostDir := t.TempDir(), t.TempDir()
	c, err := StartCore(CoreOptions{Inst: inst, Port: 0, NoFleet: true, DB: testDB(t), Keys: testKeys(t), OwnerToken: "owner-token-0123456789", NoMirror: true,
		WorkdirRoot: coreDir,
		Roster: func() []BusAgent {
			return []BusAgent{{ID: "atlas", Unit: "sales", Running: true}, {ID: "nova", Unit: "media", Running: true}}
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", c.Addr.Port)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// atlas runs on the vps (its link says so); nova does not.
	go link.NewAgent(link.AgentOptions{Agent: "atlas", Placement: "vps", CoreURL: base, CoreKey: atlasKey, Runtime: hermes.NewRuntime(),
		Spool: &link.Spool{Dir: t.TempDir()}, Listen: fmt.Sprintf("127.0.0.1:%d", freePort(t)), Logf: t.Logf}).Run(ctx)
	eventually(t, "atlas's link", func() bool { return c.Links.Connected("atlas") })
	token, _, _ := c.DB.AddToken("vps-runner", ScopeOperate, 0)

	write := func(dir, rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(dir, rel string) string {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return "<none>"
		}
		return string(b)
	}
	var logs []string
	s := &link.Syncer{CoreURL: base, Token: token, Placement: "vps", Dir: hostDir,
		Layers:    func() []string { return []string{"group", "sales", "media"} },
		StateFile: filepath.Join(t.TempDir(), "state.json"), Logf: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }}
	once := func() {
		t.Helper()
		if err := s.Once(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// First round: the core's files come down, the host's go up; media is not this host's.
	write(coreDir, "sales/plan.md", "v1")
	write(coreDir, "media/secret.md", "not for the vps")
	write(hostDir, "group/notes.md", "from the vps")
	once()
	if read(hostDir, "sales/plan.md") != "v1" || read(coreDir, "group/notes.md") != "from the vps" || read(hostDir, "media/secret.md") != "<none>" {
		t.Fatalf("first round: host plan %q, core notes %q, host media %q", read(hostDir, "sales/plan.md"), read(coreDir, "group/notes.md"), read(hostDir, "media/secret.md"))
	}

	// One side changes: it wins, either way; deletions travel too.
	write(hostDir, "sales/plan.md", "v2 on the vps")
	write(coreDir, "group/notes.md", "edited on the core")
	once()
	if read(coreDir, "sales/plan.md") != "v2 on the vps" || read(hostDir, "group/notes.md") != "edited on the core" {
		t.Errorf("one side: core plan %q, host notes %q", read(coreDir, "sales/plan.md"), read(hostDir, "group/notes.md"))
	}
	os.Remove(filepath.Join(coreDir, "group", "notes.md"))
	once()
	if read(hostDir, "group/notes.md") != "<none>" {
		t.Error("a deletion on the core did not reach the host")
	}

	// Both change: the core's version stays, the host's comes back as a conflict copy on both.
	write(hostDir, "sales/plan.md", "v3 vps")
	write(coreDir, "sales/plan.md", "v3 core")
	once()
	if read(hostDir, "sales/plan.md") != "v3 core" || read(coreDir, "sales/plan.md") != "v3 core" {
		t.Errorf("conflict: host %q core %q", read(hostDir, "sales/plan.md"), read(coreDir, "sales/plan.md"))
	}
	matches, _ := filepath.Glob(filepath.Join(coreDir, "sales", "plan.conflict-vps-*.md"))
	if len(matches) != 1 || read(coreDir, "sales/"+filepath.Base(matches[0])) != "v3 vps" {
		t.Errorf("conflict copy on the core: %v", matches)
	}

	// Another agent's hard lock: the host's change is kept as a copy, not written over it.
	if _, err := c.Workdir.Lock("nova", "/shared/group/brief.md", "hard", "editing", 30); err != nil {
		t.Fatal(err)
	}
	write(hostDir, "group/brief.md", "vps draft")
	once()
	if read(coreDir, "group/brief.md") != "<none>" {
		t.Error("written under nova's hard lock")
	}
	if !strings.Contains(strings.Join(logs, "\n"), "group/brief.md changed on both sides") {
		t.Errorf("logs: %v", logs)
	}
}
