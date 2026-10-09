package agentpack

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/secrets"
)

// acme is a scratch copy of the example instance.
func acme(t *testing.T) *instance.Instance {
	t.Helper()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("../../examples/minimal")); err != nil {
		t.Fatal(err)
	}
	inst, err := instance.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

func write(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func checks(inst *instance.Instance) func(string) error {
	return func(id string) error { _, err := manifest.Load(inst.Root, id, inst.Names.Secret); return err }
}

func TestConfigExportCarriesNoData(t *testing.T) {
	src := acme(t)
	write(t, secrets.Path(src.Root), "shared:\n  OPENROUTER_API_KEY: or-key\n")
	write(t, filepath.Join(manifest.DataDir(src.Root, "atlas"), "store", "latest.json"), `{"nap":"n1"}`)
	out := filepath.Join(t.TempDir(), "atlas.zip")
	r, err := Export(src, "atlas", false, out)
	if err != nil {
		t.Fatal(err)
	}
	m, members, err := Read(out)
	if err != nil {
		t.Fatal(err)
	}
	if r.ContainsSecrets || m.ContainsSecrets || m.Mode != "config" || m.Persona != "atlas" {
		t.Errorf("manifest %+v", m)
	}
	for p := range members {
		if strings.HasPrefix(p, "data/") || strings.Contains(p, "/data/") || p == Disclaimer {
			t.Errorf("a config export holds %s", p)
		}
	}
	for _, p := range []string{"agent/agent.yaml", "agent/SOUL.md", "agent/engine/hermes/config.yaml", "persona/atlas/persona.md", "unit.json", "actions.yaml"} {
		if _, ok := members[p]; !ok {
			t.Errorf("missing %s", p)
		}
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o644 {
		t.Errorf("mode %v", info.Mode().Perm())
	}
}

func TestDataRoundTrip(t *testing.T) {
	src := acme(t)
	write(t, secrets.Path(src.Root), "shared:\n  OPENROUTER_API_KEY: or-key\n")
	write(t, secrets.AgentFile(src.Root, "atlas"), "secrets:\n  API_SERVER_KEY: atlas-api-key-0123456789\n  CRM_API_TOKEN: crm\nlocal:\n  SWARM_CORE_KEY: core-key\n")
	store := filepath.Join(manifest.DataDir(src.Root, "atlas"), "store")
	write(t, filepath.Join(store, "latest.json"), `{"nap":"n1"}`)
	write(t, filepath.Join(store, "naps", "n1.json"), `{"id":"n1","agent":"atlas","files":[]}`)
	out := filepath.Join(t.TempDir(), "atlas.zip")
	r, err := Export(src, "atlas", true, out)
	if err != nil {
		t.Fatal(err)
	}
	m, members, err := Read(out)
	if err != nil {
		t.Fatal(err)
	}
	if !r.ContainsSecrets || !m.ContainsSecrets || m.Mode != "data" || members[Disclaimer] == nil || members["data/store/latest.json"] == nil {
		t.Fatalf("manifest %+v", m)
	}
	if s := string(members["data/secrets.yaml"]); !strings.Contains(s, "or-key") || !strings.Contains(s, "atlas-api-key") || !strings.Contains(s, "core-key") {
		t.Errorf("secrets.yaml:\n%s", s)
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Errorf("a data export is %v, want 0600", info.Mode().Perm())
	}

	// Into another instance, as another agent: its shared OPENROUTER_API_KEY stays its own.
	dst := acme(t)
	write(t, secrets.Path(dst.Root), "shared:\n  OPENROUTER_API_KEY: theirs\n")
	if err := os.RemoveAll(manifest.AgentDir(dst.Root, "atlas")); err != nil {
		t.Fatal(err)
	}
	ir, err := Import(dst, out, ImportOptions{As: "orion"}, checks(dst))
	if err != nil {
		t.Fatal(err)
	}
	a, err := manifest.Load(dst.Root, "orion", dst.Names.Secret)
	if err != nil || a.ID != "orion" || a.Unit != "sales" {
		t.Fatalf("%+v %v", a, err)
	}
	f, _ := secrets.Load(secrets.Path(dst.Root))
	if v, _ := f.Shared.Get("OPENROUTER_API_KEY"); v != "theirs" {
		t.Errorf("shared value overwritten: %q", v)
	}
	if v, _ := f.Agents.Get("orion").Get("API_SERVER_KEY"); v != "atlas-api-key-0123456789" || !f.InFolder("orion") {
		t.Errorf("own value %q, in folder %v", v, f.InFolder("orion"))
	}
	if _, err := os.Stat(filepath.Join(manifest.DataDir(dst.Root, "orion"), "store", "naps", "n1.json")); err != nil {
		t.Error(err)
	}
	if !strings.Contains(strings.Join(ir.Changes, "\n"), "left to this instance's shared or unit layer: OPENROUTER_API_KEY") {
		t.Errorf("changes %v", ir.Changes)
	}
	// Again: refused without --replace; an import that does not check leaves nothing behind.
	if _, err := Import(dst, out, ImportOptions{As: "orion"}, checks(dst)); err == nil || err.(*Error).Code != "exists" {
		t.Errorf("duplicate: %v", err)
	}
	before, _ := os.ReadFile(filepath.Join(manifest.AgentDir(dst.Root, "orion"), "agent.yaml"))
	if _, err := Import(dst, out, ImportOptions{As: "orion", Replace: true}, func(string) error { return os.ErrInvalid }); err == nil {
		t.Fatal("a failing check must fail the import")
	}
	after, err := os.ReadFile(filepath.Join(manifest.AgentDir(dst.Root, "orion"), "agent.yaml"))
	if err != nil || string(after) != string(before) {
		t.Errorf("rollback left %v", err)
	}
	if _, err := os.Stat(filepath.Join(manifest.DataDir(dst.Root, "orion"), "store", "naps", "n1.json")); err != nil {
		t.Errorf("rollback lost the data: %v", err)
	}
}

func TestImportRefusals(t *testing.T) {
	src := acme(t)
	out := filepath.Join(t.TempDir(), "atlas.zip")
	if _, err := Export(src, "atlas", false, out); err != nil {
		t.Fatal(err)
	}
	dst := acme(t)
	if _, err := Import(dst, out, ImportOptions{As: "orion", Unit: "nowhere"}, nil); err == nil || err.(*Error).Code != "unknown_unit" {
		t.Errorf("unit: %v", err)
	}
	// The destination lacks the agent's bridge action: refused unless --with-actions.
	write(t, filepath.Join(dst.Root, dst.BridgeActions), "# none yet\n[]\n")
	if _, err := Import(dst, out, ImportOptions{As: "orion"}, nil); err == nil || err.(*Error).Code != "missing_actions" {
		t.Errorf("actions: %v", err)
	}
	if _, err := Import(dst, out, ImportOptions{As: "orion", WithActions: true}, checks(dst)); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst.Root, dst.BridgeActions)); !strings.Contains(string(b), "sales.crm.deals.list") {
		t.Errorf("actions file:\n%s", b)
	}
	// A member path that leaves the zip is refused before anything is read into the instance.
	evil := filepath.Join(t.TempDir(), "evil.zip")
	f, _ := os.Create(evil)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../outside")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	if _, _, err := Read(evil); err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Errorf("evil: %v", err)
	}
}

func TestFormatZeroIsNotExported(t *testing.T) {
	src := acme(t)
	write(t, filepath.Join(manifest.AgentDir(src.Root, "nova"), "agent.yaml"),
		"id: nova\nname: Nova\nunit: support\nrole: r\nengine:\n  kind: hermes\n  version: 0.21.5\n  model: x\n")
	if _, err := Export(src, "nova", false, filepath.Join(t.TempDir(), "n.zip")); err == nil || err.(*Error).Code != "legacy" {
		t.Errorf("format 0: %v", err)
	}
}
