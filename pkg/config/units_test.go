package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestUnits(t *testing.T) {
	inst := acme(t)
	units := Units(inst)
	ids := []string{}
	for _, u := range units {
		ids = append(ids, u.ID)
	}
	if !slices.Equal(ids, []string{"group", "sales", "support"}) {
		t.Fatalf("units %v", ids)
	}
	group, sales := units[0], units[1]
	if len(group.Agents) != 3 || !slices.Equal(sales.Agents, []string{"atlas", "scout"}) {
		t.Errorf("members: group %v, sales %v", group.Agents, sales.Agents)
	}
	if sales.Name != "Acme Sales" || sales.Path != "units/sales/unit.yaml" || sales.Problem != "" {
		t.Errorf("%+v", sales)
	}
	if len(sales.Knowledge) != 1 || sales.Knowledge[0].Name != "README.md" || sales.Knowledge[0].Compiled {
		t.Errorf("knowledge %+v", sales.Knowledge)
	}

	// A unit.yaml that does not load is listed with its problem, to be fixed as text.
	if err := os.WriteFile(filepath.Join(inst.Root, "units/support/unit.yaml"), []byte("id: other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if u := Units(inst)[2]; u.Problem == "" || u.Name != "support" {
		t.Errorf("broken unit: %+v", u)
	}
}

func TestUnitFile(t *testing.T) {
	inst := acme(t)
	rel := "units/sales/unit.yaml"
	if err := os.WriteFile(filepath.Join(inst.Root, rel), []byte("# kept\nid: sales\nname: Acme Sales # inline\ndescription: Deals.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Show(inst, rel)
	if err != nil || f.Kind != "unit" || f.Unit != "sales" || f.Agent != "" || f.Options != nil {
		t.Fatal(f, err)
	}
	if doc, _ := f.Doc.(map[string]any); doc["name"] != "Acme Sales" {
		t.Fatalf("doc %v", f.Doc)
	}
	w, err := Apply(inst, rel, []byte(`{"description":"Inbound sales."}`), f.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if want := "# kept\nid: sales\nname: Acme Sales # inline\ndescription: Inbound sales.\n"; w.Text != want || onDisk(t, inst, rel) != want {
		t.Errorf("after apply %q", w.Text)
	}
	for patch, want := range map[string]string{`{"id":"deals"}`: "readonly", `{"name":""}`: "invalid", `{"name":null}`: "invalid"} {
		if _, err := Apply(inst, rel, []byte(patch), w.Hash); code(err) != want {
			t.Errorf("%s: want %s, got %v", patch, want, err)
		}
	}
	if _, err := Write(inst, rel, []byte("id: deals\nname: Deals\n"), w.Hash); code(err) != "invalid" {
		t.Errorf("renamed id: %v", err)
	}
	if _, err := Write(inst, rel, []byte("id: [\n"), w.Hash); code(err) != "invalid" {
		t.Errorf("bad yaml: %v", err)
	}
	if onDisk(t, inst, rel) != w.Text {
		t.Error("a refused write changed the file")
	}
}

func TestKnowledge(t *testing.T) {
	inst := acme(t)
	rel := "units/sales/knowledge/pricing.md"
	f, err := Create(inst, rel, []byte("Quotes are valid for 7 days.\n"))
	if err != nil || f.Kind != "knowledge" || f.Unit != "sales" || f.Doc != nil {
		t.Fatal(f, err)
	}
	if _, err := Create(inst, rel, []byte("again\n")); code(err) != "exists" {
		t.Errorf("create over an existing file: %v", err)
	}
	if onDisk(t, inst, rel) != "Quotes are valid for 7 days.\n" {
		t.Error("a refused create changed the file")
	}
	w, err := Write(inst, rel, []byte("Quotes are valid for 10 days.\n"), f.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if k := Units(inst)[1].Knowledge; len(k) != 2 || k[1].Name != "pricing.md" || !k[1].Compiled {
		t.Errorf("listed %+v", k)
	}
	if _, err := Apply(inst, rel, []byte(`{}`), w.Hash); code(err) != "unsupported" {
		t.Errorf("knowledge patch: %v", err)
	}
	if err := Delete(inst, rel, f.Hash); code(err) != "conflict" {
		t.Errorf("stale delete: %v", err)
	}
	if err := Delete(inst, "units/sales/unit.yaml", "x"); code(err) != "unsupported" {
		t.Errorf("deleting a unit.yaml: %v", err)
	}
	if err := Delete(inst, rel, w.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inst.Root, rel)); !os.IsNotExist(err) {
		t.Errorf("still there: %v", err)
	}
	for _, rel := range []string{"units/ghost/knowledge/a.md", "units/sales/knowledge/a.txt", "agents/atlas/SOUL.md"} {
		if _, err := Create(inst, rel, []byte("x\n")); code(err) != "unsupported" {
			t.Errorf("create %s: %v", rel, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(inst.Root, "units/sales/knowledge"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}

func TestCreateUnit(t *testing.T) {
	inst := acme(t)
	f, err := Create(inst, "units/billing/unit.yaml", []byte("id: billing\nname: Acme Billing\ndescription: Invoices.\n"))
	if err != nil || f.Kind != "unit" {
		t.Fatal(f, err)
	}
	if u := Units(inst); len(u) != 4 || u[1].ID != "billing" || len(u[1].Agents) != 0 {
		t.Errorf("after create: %+v", u)
	}
	// It can now take knowledge.
	if _, err := Create(inst, "units/billing/knowledge/terms.md", []byte("Net 30.\n")); err != nil {
		t.Fatal(err)
	}
	for rel, c := range map[string]struct{ body, code string }{
		"units/sales/unit.yaml":    {"id: sales\nname: S\n", "exists"},
		"units/Bad_Id/unit.yaml":   {"id: Bad_Id\nname: B\n", "invalid"},
		"units/legal/unit.yaml":    {"id: law\nname: Legal\n", "invalid"},
		"units/nameless/unit.yaml": {"id: nameless\n", "invalid"},
	} {
		if _, err := Create(inst, rel, []byte(c.body)); code(err) != c.code {
			t.Errorf("%s: want %s, got %v", rel, c.code, err)
		}
	}
	for _, dir := range []string{"legal", "nameless", "Bad_Id"} {
		if _, err := os.Stat(filepath.Join(inst.Root, "units", dir)); !os.IsNotExist(err) {
			t.Errorf("a refused create left units/%s", dir)
		}
	}
}
