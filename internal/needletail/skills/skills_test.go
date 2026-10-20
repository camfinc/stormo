package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexGoldenHome(t *testing.T) {
	home := filepath.Join("..", "..", "..", "pkg", "engine", "needletail", "testdata", "home")
	idx := Index(home)
	if len(idx) != 8 {
		t.Fatalf("index = %+v", idx)
	}
	found := false
	for _, s := range idx {
		if s.Name == "crm-api" {
			found = s.Path == "skills/crm/crm-api/SKILL.md" && s.Description == "Query Acme deals from the CRM API ($CRM_API_TOKEN)."
		}
	}
	if !found {
		t.Errorf("crm-api = %+v", idx)
	}
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "skills", ".hub", "x"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "skills", ".hub", "x", "SKILL.md"), []byte("---\nname: hidden\n---\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(dir, "skills", "plain"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "skills", "plain", "SKILL.md"), []byte("# no front matter\n"), 0o644)
	if idx := Index(dir); len(idx) != 1 || idx[0].Name != "plain" {
		t.Errorf("index = %+v", idx)
	}
}
