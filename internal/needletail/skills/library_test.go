package skills

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func put(t *testing.T, home, rel, body string) {
	t.Helper()
	p := filepath.Join(home, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// library is a home with a repo skill (with files and a nested skill), a generated knowledge
// skill and a skill without front matter.
func library(t *testing.T) *Library {
	t.Helper()
	home := t.TempDir()
	put(t, home, "skills/crm/crm-api/SKILL.md", "---\nname: crm-api\ndescription: Query deals.\n---\nRun python3 ${SKILL_DIR}/scripts/deals.py\n")
	put(t, home, "skills/crm/crm-api/scripts/deals.py", "print('deals')\n")
	put(t, home, "skills/crm/crm-api/references/api.md", "See ${SKILL_DIR}/scripts.\n")
	put(t, home, "skills/crm/crm-api/.cache/x", "hidden")
	put(t, home, "skills/crm/crm-api/quotes/SKILL.md", "---\nname: quotes\ndescription: Quotes.\n---\n")
	put(t, home, "skills/crm/crm-api/quotes/q.md", "nested")
	put(t, home, "skills/acme-knowledge/SKILL.md", "---\nname: acme-knowledge\ndescription: What Acme knows.\n---\n")
	put(t, home, "skills/research/plain/SKILL.md", "# no front matter\n")
	put(t, home, "skills/solo/SKILL.md", "---\nname: solo\ndescription: A top-level skill.\n---\n")
	clock := time.Date(2026, 10, 21, 9, 30, 0, 123456000, time.UTC)
	return &Library{Home: home, Generated: []string{"acme-knowledge", "acme-shared-docs"}, Now: func() time.Time { return clock }}
}

func TestFind(t *testing.T) {
	l := library(t)
	for _, name := range []string{"crm-api", "crm/crm-api", "skills/crm/crm-api", "skills/crm/crm-api/SKILL.md", filepath.Join(l.Home, "skills", "crm", "crm-api"), " crm/crm-api/ "} {
		if s, err := l.Find(name); err != nil || s.Dir != "crm/crm-api" || s.Name != "crm-api" {
			t.Errorf("Find(%q) = %+v %v", name, s, err)
		}
	}
	if s, err := l.Find("plain"); err != nil || s.Dir != "research/plain" {
		t.Errorf("plain = %+v %v", s, err)
	}
	if _, err := l.Find("ghost"); err == nil || !strings.Contains(err.Error(), "skills_list") {
		t.Errorf("ghost: %v", err)
	}
	put(t, l.Home, "skills/other/crm-api/SKILL.md", "---\nname: crm-api\ndescription: Another.\n---\n")
	if _, err := l.Find("crm-api"); !errors.Is(err, ErrAmbiguous) || !strings.Contains(err.Error(), "other/crm-api") {
		t.Errorf("ambiguous: %v", err)
	}
	if s, err := l.Find("other/crm-api"); err != nil || s.Dir != "other/crm-api" {
		t.Errorf("by dir = %+v %v", s, err)
	}
	if !l.ReadOnly("acme-knowledge") || !l.ReadOnly("x/acme-shared-docs") || !l.ReadOnly(".hub/x") || l.ReadOnly("crm/crm-api") {
		t.Error("ReadOnly")
	}
}

func TestView(t *testing.T) {
	l := library(t)
	v, err := l.View("s1", "crm-api", "")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(l.Home, "skills", "crm", "crm-api")
	if v.SkillDir != dir || !strings.Contains(v.Content, "python3 "+dir+"/scripts/deals.py") || strings.Contains(v.Content, "${SKILL_DIR}") {
		t.Errorf("view = %+v", v)
	}
	// hidden files and the nested skill's files are not this skill's
	if !slices.Equal(v.Files, []string{"references/api.md", "scripts/deals.py"}) || v.File != "SKILL.md" || v.ReadOnly {
		t.Errorf("files = %v", v.Files)
	}
	if s, ok := l.LastViewed("s1"); !ok || s.Dir != "crm/crm-api" {
		t.Errorf("last viewed = %+v %v", s, ok)
	}
	if _, ok := l.LastViewed("s2"); ok {
		t.Error("another session's view")
	}
	f, err := l.View("s1", "crm/crm-api", "references/api.md")
	if err != nil || f.Content != "See "+dir+"/scripts.\n" || f.Files != nil {
		t.Errorf("file view = %+v %v", f, err)
	}
	for _, bad := range []string{"../acme-knowledge/SKILL.md", "/etc/passwd", ".cache/x", "references/../../../SKILL.md", "missing.md"} {
		if _, err := l.View("s1", "crm-api", bad); err == nil {
			t.Errorf("%s: viewed", bad)
		}
	}
	put(t, l.Home, "skills/crm/crm-api/bin.dat", "a\x00b")
	if _, err := l.View("s1", "crm-api", "bin.dat"); err == nil || !strings.Contains(err.Error(), "binary") {
		t.Errorf("binary: %v", err)
	}
	// a link out of the home is never followed, at the file or the skill
	outside := t.TempDir()
	put(t, outside, "secret.txt", "nope")
	put(t, outside, "SKILL.md", "---\nname: linked\ndescription: x\n---\n")
	_ = os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.md"))
	if _, err := l.View("s1", "crm-api", "link.md"); err == nil {
		t.Error("symlinked file viewed")
	}
	_ = os.Symlink(outside, filepath.Join(l.Home, "skills", "research", "linked"))
	if _, err := l.View("s1", "research/linked", ""); err == nil {
		t.Error("symlinked skill viewed")
	}
	u, _ := l.Usage()
	if e := u["crm-api"]; e["view_count"] != float64(1) || e["use_count"] != float64(2) || e["last_viewed_at"] != "2026-10-21T09:30:00.123456+00:00" || e["created_by"] != nil || e["state"] != "active" {
		t.Errorf("usage = %v", u)
	}
}

func TestReferenced(t *testing.T) {
	l := library(t)
	if s, ok := l.Referenced("python3 " + l.Home + "/skills/crm/crm-api/scripts/deals.py --today"); !ok || s.Dir != "crm/crm-api" {
		t.Errorf("absolute = %+v %v", s, ok)
	}
	if s, ok := l.Referenced("cat skills/crm/crm-api/quotes/q.md"); !ok || s.Dir != "crm/crm-api/quotes" {
		t.Errorf("nested = %+v %v", s, ok)
	}
	if _, ok := l.Referenced("cat skills/crm/crm-api/x skills/research/plain/y"); ok {
		t.Error("two skills")
	}
	if _, ok := l.Referenced("ls skills/"); ok {
		t.Error("no skill")
	}
}

func TestManage(t *testing.T) {
	l := library(t)
	md := "---\nname: fare-lookup\ndescription: Look up a fare by route.\n---\nRun ${SKILL_DIR}/scripts/fare.sh\n"
	s, err := l.Create("fare-lookup", "crm", md)
	if err != nil || s.Dir != "crm/fare-lookup" || s.Path != "skills/crm/fare-lookup/SKILL.md" || s.Description != "Look up a fare by route." {
		t.Fatalf("create = %+v %v", s, err)
	}
	if got, _ := os.ReadFile(filepath.Join(l.Home, "skills", "crm", "fare-lookup", "SKILL.md")); string(got) != md {
		t.Errorf("SKILL.md = %q", got)
	}
	for _, c := range []struct{ name, cat, content, want string }{
		{"Fare", "", md, "lowercase"},
		{"fare", "../x", md, "category"},
		{"crm-api", "", "---\nname: crm-api\ndescription: d\n---\n", "exists"},
		{"fare-lookup", "other", md, "exists"},
		{"acme-knowledge", "mine", "---\nname: acme-knowledge\ndescription: d\n---\n", "generates"},
		{"inner", "solo", "---\nname: inner\ndescription: d\n---\n", "inside skill solo"},
		{"x", "", "no front matter", "front matter"},
		{"y", "", "---\nname: z\ndescription: d\n---\n", "renamed"},
		{"w", "", "---\nname: w\n---\n", "description"},
	} {
		_, err := l.Create(c.name, c.cat, c.content)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("create %s/%s: %v", c.cat, c.name, err)
		}
	}
	// a category named like a skill elsewhere is only a directory
	if _, err := l.Create("inner", "crm-api", "---\nname: inner\ndescription: d\n---\n"); err != nil {
		t.Errorf("create crm-api/inner: %v", err)
	}

	if _, _, err := l.WriteFile("fare-lookup", "scripts/fare.sh", "echo fare\n"); err != nil {
		t.Fatal(err)
	}
	if _, f, n, err := l.Patch("fare-lookup", "scripts/fare.sh", "fare", "price", false); err != nil || f != "scripts/fare.sh" || n != 1 {
		t.Errorf("patch = %s %d %v", f, n, err)
	}
	if _, _, _, err := l.Patch("fare-lookup", "", "${SKILL_DIR}", "x", false); err != nil {
		t.Errorf("patch SKILL.md: %v", err)
	}
	if _, _, _, err := l.Patch("fare-lookup", "", "name: fare-lookup", "name: renamed", false); err == nil || !strings.Contains(err.Error(), "renamed") {
		t.Errorf("rename by patch: %v", err)
	}
	if _, _, _, err := l.Patch("fare-lookup", "", "nothing like this", "x", false); err == nil {
		t.Error("patch of absent text")
	}
	_, _, _ = l.WriteFile("fare-lookup", "notes.md", "a a a")
	if _, _, _, err := l.Patch("fare-lookup", "notes.md", "a", "b", false); err == nil || !strings.Contains(err.Error(), "3 times") {
		t.Errorf("ambiguous patch: %v", err)
	}
	if _, _, n, err := l.Patch("fare-lookup", "notes.md", "a", "b", true); err != nil || n != 3 {
		t.Errorf("replace_all = %d %v", n, err)
	}
	for _, bad := range []string{"SKILL.md", "../crm-api/SKILL.md", "/tmp/x", ".hidden", "tools/lookup.luau", "a/../../b"} {
		if _, _, err := l.WriteFile("fare-lookup", bad, "x"); err == nil {
			t.Errorf("write_file %s accepted", bad)
		}
	}
	if _, _, err := l.WriteFile("crm-api", "quotes/q.md", "x"); err == nil || !strings.Contains(err.Error(), "belongs to skill") {
		t.Errorf("write into a nested skill: %v", err)
	}
	if _, err := l.Edit("fare-lookup", "---\nname: fare-lookup\ndescription: Fares.\n---\nNew body.\n"); err != nil {
		t.Errorf("edit: %v", err)
	}
	if _, err := l.Edit("fare-lookup", "---\nname: other\ndescription: Fares.\n---\n"); err == nil {
		t.Error("rename by edit")
	}
	// a repo skill without front matter stays editable, and may gain front matter that keeps its name
	if _, err := l.Edit("plain", "still none\n"); err != nil {
		t.Errorf("edit plain: %v", err)
	}
	if _, err := l.Edit("plain", "---\nname: plain\ndescription: Now described.\n---\n"); err != nil {
		t.Errorf("edit plain with front matter: %v", err)
	}
	// repo skills can be improved; generated ones never
	if _, _, err := l.WriteFile("crm-api", "references/learned.md", "use orderBy"); err != nil {
		t.Errorf("repo skill: %v", err)
	}
	gen := map[string]func() error{
		"edit": func() error {
			_, err := l.Edit("acme-knowledge", "---\nname: acme-knowledge\ndescription: d\n---\n")
			return err
		},
		"patch":       func() error { _, _, _, err := l.Patch("acme-knowledge", "", "What", "x", false); return err },
		"write_file":  func() error { _, _, err := l.WriteFile("acme-knowledge", "x.md", "x"); return err },
		"remove_file": func() error { _, _, err := l.RemoveFile("acme-knowledge", "x.md"); return err },
		"delete":      func() error { _, err := l.Delete("acme-knowledge"); return err },
	}
	for op, f := range gen {
		if err := f(); err == nil || !strings.Contains(err.Error(), "generated") {
			t.Errorf("%s of a generated skill: %v", op, err)
		}
	}
	if _, err := os.Stat(filepath.Join(l.Home, "skills", "acme-knowledge", "SKILL.md")); err != nil {
		t.Error("generated skill touched")
	}

	_, _, _ = l.WriteFile("fare-lookup", "deep/er/x.md", "x")
	if _, _, err := l.RemoveFile("fare-lookup", "deep/er/x.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.Home, "skills", "crm", "fare-lookup", "deep")); !os.IsNotExist(err) {
		t.Error("empty dirs left behind")
	}
	if _, _, err := l.RemoveFile("fare-lookup", "SKILL.md"); err == nil {
		t.Error("removed SKILL.md")
	}
	if _, err := l.Delete("crm-api"); err == nil || !strings.Contains(err.Error(), "quotes") {
		t.Errorf("delete of a skill holding another: %v", err)
	}

	u, _ := l.Usage()
	e := u["fare-lookup"]
	// write_file ×3, patch ×2 and replace_all (the refused ones not counted), edit, remove_file
	if e["created_by"] != "agent" || e["patch_count"] != float64(8) || e["last_patched_at"] != "2026-10-21T09:30:00.123456+00:00" || e["view_count"] != float64(0) {
		t.Errorf("usage = %v", e)
	}
	if _, err := l.Delete("fare-lookup"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.Home, "skills", "crm", "fare-lookup")); !os.IsNotExist(err) {
		t.Error("not deleted")
	}
	if u, _ := l.Usage(); u["fare-lookup"] != nil || u["crm-api"] == nil {
		t.Errorf("usage after delete = %v", u)
	}
}

// A usage file from Hermes (an agent that switched engines) keeps the keys needletail does not
// maintain; an unreadable one is started again.
func TestUsageKeepsHermesKeys(t *testing.T) {
	l := library(t)
	put(t, l.Home, UsageFile, `{"crm-api":{"archived_at":null,"created_at":"2026-10-12T13:30:46.786811+00:00","created_by":null,"patch_generation":2,"pinned":true,"state":"active","use_count":21,"view_count":21}}`)
	s, _ := l.Find("crm-api")
	l.Used(s)
	raw, _ := os.ReadFile(filepath.Join(l.Home, UsageFile))
	var u map[string]map[string]any
	if err := json.Unmarshal(raw, &u); err != nil {
		t.Fatal(err)
	}
	e := u["crm-api"]
	if e["use_count"] != float64(22) || e["view_count"] != float64(21) || e["pinned"] != true || e["patch_generation"] != float64(2) ||
		e["created_at"] != "2026-10-12T13:30:46.786811+00:00" || e["last_used_at"] != "2026-10-21T09:30:00.123456+00:00" {
		t.Errorf("entry = %v", e)
	}
	put(t, l.Home, UsageFile, "{torn")
	l.Used(s)
	if u, _ := l.Usage(); u["crm-api"]["use_count"] != float64(1) {
		t.Errorf("after a torn file = %v", u)
	}
}
