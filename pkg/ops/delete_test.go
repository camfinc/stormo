package ops

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/aws"
	"github.com/camfinc/stormo/pkg/deploy"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
)

// scratch copies the example instance, with a secret value per agent, and fakes ECS: running
// names the agents whose service runs.
func scratch(t *testing.T, running ...string) *Deps {
	root := filepath.Join(t.TempDir(), "acme")
	if out, err := exec.Command("cp", "-R", "../../examples/minimal", root).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	body := "agents:\n  atlas:\n    APIFY_API_TOKEN: a\n  nova:\n    API_SERVER_KEY: n\nlocal:\n  agents:\n    nova:\n      SWARM_CORE_KEY: k\n"
	if err := os.WriteFile(secrets.Path(root), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	cli := func(args []string, _ *string) aws.Result {
		var svcs []any
		for _, id := range running {
			if slices.Contains(args, "acme-"+id) || strings.Contains(strings.Join(args, " "), id) {
				svcs = append(svcs, map[string]any{"status": "ACTIVE", "desiredCount": 1, "runningCount": 1})
			}
		}
		b, _ := json.Marshal(map[string]any{"services": svcs})
		return aws.Result{Stdout: string(b)}
	}
	return &Deps{Inst: inst, AWS: cli, Target: deploy.DefaultTarget(inst), Log: func(string) {}}
}

func TestDeleteAgentRemovesItsFolderSecretsAndOwnPersona(t *testing.T) {
	d := scratch(t)
	root := d.Inst.Root
	r, err := DeleteAgent(d, "atlas", DeleteOptions{Persona: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"agents/atlas", "personas/atlas"} {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s still there", p)
		}
	}
	if slices.Contains(manifest.AgentIDs(root), "atlas") || !slices.Contains(manifest.AgentIDs(root), "nova") {
		t.Errorf("agents now %v", manifest.AgentIDs(root))
	}
	f, err := secrets.Load(secrets.Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if f.Agents.Get("atlas") != nil || f.Agents.Get("nova") == nil || f.Local.Agents.Get("nova") == nil {
		t.Errorf("secret layers: atlas %v, nova %v / %v", f.Agents.Get("atlas"), f.Agents.Get("nova"), f.Local.Agents.Get("nova"))
	}
	if !slices.Contains(r.Removed, "agents/atlas") || !slices.Contains(r.Removed, "personas/atlas") {
		t.Errorf("removed %v", r.Removed)
	}
	if _, err := DeleteAgent(d, "atlas", DeleteOptions{}); err == nil {
		t.Error("deleted an agent that is gone")
	}
}

func TestDeleteAgentKeepsASharedPersonaAndRefusesWhileOnECS(t *testing.T) {
	d := scratch(t, "nova")
	root := d.Inst.Root
	if _, err := DeleteAgent(d, "nova", DeleteOptions{}); err == nil || !strings.Contains(err.Error(), "ECS") {
		t.Fatalf("err = %v, want a refusal while it runs on ECS", err)
	}
	if _, err := os.Stat(filepath.Join(root, "agents/nova/agent.yaml")); err != nil {
		t.Fatal("refused delete removed files")
	}
	// scout shares atlas's persona: deleting atlas with its persona keeps it.
	scout := filepath.Join(root, "agents/scout/agent.yaml")
	b, _ := os.ReadFile(scout)
	if err := os.WriteFile(scout, []byte(strings.Replace(string(b), "unit: sales\n", "unit: sales\npersona:\n  path: personas/atlas\n", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := DeleteAgent(d, "atlas", DeleteOptions{Persona: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "personas/atlas")); err != nil || !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "scout uses it") }) {
		t.Errorf("shared persona: %v, notes %v", err, r.Notes)
	}
}
