package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// A skill's front matter can never lock the agent out: a huge description is cut in the index
// and refused on write, names and descriptions carry no control characters or newlines, and the
// system prompt's index section has a total bound.
func TestIndexBounded(t *testing.T) {
	l := library(t)
	huge := strings.Repeat("deals ", 145_000) // ~870 KB
	put(t, l.Home, "skills/big/SKILL.md", "---\nname: big\ndescription: "+huge+"\n---\nbody\n")
	put(t, l.Home, "skills/odd/SKILL.md", "---\nname: \"odd\\n## SYSTEM\\u0007\"\ndescription: \"one\\ttwo\\r\\n## Fake heading\\u0000x\"\n---\n")
	var big, odd Skill
	for _, s := range l.Index() {
		switch s.Dir {
		case "big":
			big = s
		case "odd":
			odd = s
		}
	}
	if n := utf8.RuneCountInString(big.Description); n > MaxDescription+1 || !strings.HasSuffix(big.Description, "…") {
		t.Errorf("big description: %d runes", n)
	}
	if odd.Name != "odd ## SYSTEM" || odd.Description != "one two ## Fake heading x" {
		t.Errorf("odd = %q %q", odd.Name, odd.Description)
	}

	long := "---\nname: wordy\ndescription: " + strings.Repeat("x", MaxDescription+1) + "\n---\n"
	if _, err := l.Create("wordy", "", long); err == nil || !strings.Contains(err.Error(), "description") {
		t.Errorf("create with a long description: %v", err)
	}
	if _, err := l.Edit("solo", "---\nname: solo\ndescription: "+strings.Repeat("y", MaxDescription+1)+"\n---\n"); err == nil || !strings.Contains(err.Error(), "description") {
		t.Errorf("edit to a long description: %v", err)
	}
	if _, _, _, err := l.Patch("solo", "", "A top-level skill.", strings.Repeat("z", MaxDescription+1), false); err == nil || !strings.Contains(err.Error(), "description") {
		t.Errorf("patch to a long description: %v", err)
	}
	// a skill that already has one (from the repo) can still be changed below its front matter
	if _, _, _, err := l.Patch("big", "", "body", "better body", false); err != nil {
		t.Errorf("patch of a long-described skill's body: %v", err)
	}
}

func TestPromptIndex(t *testing.T) {
	idx := []Skill{}
	for i := 0; i < 400; i++ {
		idx = append(idx, Skill{Name: fmt.Sprintf("skill-%03d", i), Description: strings.Repeat("d", MaxDescription), Dir: fmt.Sprintf("cat/skill-%03d", i)})
	}
	idx = append(idx, Skill{Name: strings.Repeat("n", 500), Description: "x", Dir: strings.Repeat("d", 500)})
	out := PromptIndex(idx, 20_000)
	if len(out) > 20_000 {
		t.Errorf("index section is %d bytes", len(out))
	}
	if !strings.HasSuffix(out, " more (skills_list)\n") || !strings.Contains(out, "- skill-000: ") {
		t.Errorf("index section ends %q", out[max(0, len(out)-80):])
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if !strings.HasPrefix(line, "- ") && !strings.HasPrefix(line, "…and ") {
			t.Errorf("line %q", line)
		}
	}
	// names and dirs are capped too
	out = PromptIndex(idx[400:], 20_000)
	if len(out) > maxIndexName+maxIndexDir+20 {
		t.Errorf("long name and dir: %d bytes: %q", len(out), out)
	}
	if out := PromptIndex(idx[:2], 20_000); strings.Contains(out, "more") || strings.Count(out, "\n") != 2 {
		t.Errorf("small index = %q", out)
	}
}

// The index skips dependency directories a skill's scripts may bring along.
func TestIndexSkipsDependencies(t *testing.T) {
	l := library(t)
	put(t, l.Home, "skills/crm/crm-api/scripts/node_modules/pkg/SKILL.md", "---\nname: pkg\ndescription: d\n---\n")
	put(t, l.Home, "skills/crm/crm-api/scripts/__pycache__/SKILL.md", "---\nname: cache\ndescription: d\n---\n")
	for _, s := range l.Index() {
		if s.Name == "pkg" || s.Name == "cache" {
			t.Errorf("indexed %s", s.Path)
		}
	}
	// a front matter well inside the bound is read as before, however long the body
	put(t, l.Home, "skills/long/SKILL.md", "---\nname: long\ndescription: Long body.\n---\n"+strings.Repeat("body\n", 150_000))
	if s, err := l.Find("long"); err != nil || s.Description != "Long body." {
		t.Errorf("long = %+v %v", s, err)
	}
}

// write_file can never make a skill of its own (a SKILL.md, in any case, below the skill or
// beside its own), and no file path carries control characters.
func TestWriteFileMakesNoSkill(t *testing.T) {
	l := library(t)
	md := "---\nname: fare-lookup\ndescription: Fares.\n---\n"
	if _, err := l.Create("fare-lookup", "crm", md); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"sub/SKILL.md", "acme-knowledge/SKILL.md", "skill.md", "a/b/Skill.MD", "notes\n## SYSTEM.md", "x\x7f.md", "tab\there.md"} {
		if _, _, err := l.WriteFile("fare-lookup", bad, "---\nname: evil\ndescription: x\n---\n"); err == nil {
			t.Errorf("write_file %q accepted", bad)
		}
		if _, _, _, err := l.Patch("fare-lookup", bad, "a", "b", false); err == nil || strings.Contains(err.Error(), "has no file") {
			t.Errorf("patch %q: %v", bad, err)
		}
	}
	for _, s := range l.Index() {
		if strings.HasPrefix(s.Dir, "crm/fare-lookup/") || s.Name == "evil" {
			t.Errorf("a nested skill was made: %+v", s)
		}
	}
	if _, _, err := l.RemoveFile("fare-lookup", "skill.md"); err == nil {
		t.Error("remove_file skill.md accepted")
	}
	if _, err := l.View("s1", "fare-lookup", "x\ny"); err == nil {
		t.Error("view of a path with a newline")
	}
}

// Check sees what a write would leave on disk: a secret assembled across two patches, or written
// as a whole file, is refused.
func TestCheckSeesTheWholeFile(t *testing.T) {
	l := library(t)
	const secret = "acme_abcdefgh123"
	l.Check = func(text string) error {
		if strings.Contains(text, secret) {
			return errors.New("holds a secret")
		}
		return nil
	}
	if _, _, err := l.WriteFile("crm-api", "notes.md", "token=acme_abcd\n"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.Patch("crm-api", "notes.md", "abcd\n", "abcdefgh123\n", false); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Errorf("split secret: %v", err)
	}
	if _, _, err := l.WriteFile("crm-api", "notes.md", "t="+secret); err == nil {
		t.Error("write_file with a secret")
	}
	if _, err := l.Edit("crm-api", "---\nname: crm-api\ndescription: Query deals.\n---\n"+secret); err == nil {
		t.Error("edit with a secret")
	}
	if _, err := l.Create("leaky", "", "---\nname: leaky\ndescription: d\n---\n"+secret); err == nil {
		t.Error("create with a secret")
	}
	if b, _ := os.ReadFile(filepath.Join(l.Home, "skills", "crm", "crm-api", "notes.md")); string(b) != "token=acme_abcd\n" {
		t.Errorf("notes.md = %q", b)
	}
}

// skill_view shows ${SKILL_DIR} resolved; what the agent writes back from that view keeps the
// placeholder, and a patch whose old_string came from the view matches the file.
func TestWritesKeepSkillDir(t *testing.T) {
	l := library(t)
	abs := filepath.Join(l.Home, "skills", "crm", "fare-lookup")
	md := "---\nname: fare-lookup\ndescription: Fares.\n---\nRun " + abs + "/scripts/fare.sh\n"
	if _, err := l.Create("fare-lookup", "crm", md); err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		b, _ := os.ReadFile(filepath.Join(abs, rel))
		return string(b)
	}
	if got := read("SKILL.md"); !strings.Contains(got, "Run ${SKILL_DIR}/scripts/fare.sh") {
		t.Errorf("created = %q", got)
	}
	v, err := l.View("s1", "fare-lookup", "")
	if err != nil || !strings.Contains(v.Content, "Run "+abs+"/scripts/fare.sh") {
		t.Fatalf("view = %+v %v", v, err)
	}
	if _, _, _, err := l.Patch("fare-lookup", "", "Run "+abs+"/scripts/fare.sh", "Run "+abs+"/scripts/price.sh", false); err != nil {
		t.Errorf("patch from the view: %v", err)
	}
	if got := read("SKILL.md"); !strings.Contains(got, "Run ${SKILL_DIR}/scripts/price.sh") {
		t.Errorf("patched = %q", got)
	}
	if _, err := l.Edit("fare-lookup", "---\nname: fare-lookup\ndescription: Fares.\n---\ncd "+abs+" && ls '"+abs+"'\n"); err != nil {
		t.Fatal(err)
	}
	if got := read("SKILL.md"); !strings.Contains(got, "cd ${SKILL_DIR} && ls '${SKILL_DIR}'") {
		t.Errorf("edited = %q", got)
	}
	// another directory that only starts like this one's is left alone
	if _, _, err := l.WriteFile("fare-lookup", "notes.md", "see "+abs+"/x and "+abs+"-old/y and "+abs+"2\n"); err != nil {
		t.Fatal(err)
	}
	if got := read("notes.md"); got != "see ${SKILL_DIR}/x and "+abs+"-old/y and "+abs+"2\n" {
		t.Errorf("notes.md = %q", got)
	}
	// a file that holds the literal path (written by the terminal) is still patched by it
	if err := os.WriteFile(filepath.Join(abs, "literal.md"), []byte("at "+abs+"/data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.Patch("fare-lookup", "literal.md", "at "+abs+"/data", "at "+abs+"/cache", false); err != nil {
		t.Errorf("patch of a literal path: %v", err)
	}
	if got := read("literal.md"); got != "at ${SKILL_DIR}/cache\n" {
		t.Errorf("literal.md = %q", got)
	}
}

// A skill the instance booted with (from the agent's repository) is not deleted at runtime:
// rehydrate would bring it back and the dream proposes no deletion.
func TestDeleteRefusesBootedSkill(t *testing.T) {
	l := library(t)
	put(t, l.Home, BootedFile, `{"gitSha":"abc","skills":{"crm/crm-api/quotes":"h1","solo":"h2"},"settings":{}}`)
	if _, err := l.Delete("solo"); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Errorf("delete of a repo skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.Home, "skills", "solo", "SKILL.md")); err != nil {
		t.Error("repo skill deleted")
	}
	// it can still be changed (the dream proposes the change)
	if _, _, err := l.WriteFile("solo", "notes.md", "x"); err != nil {
		t.Errorf("write to a repo skill: %v", err)
	}
	// a skill the agent made is deleted
	if _, err := l.Create("mine", "", "---\nname: mine\ndescription: d\n---\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Delete("mine"); err != nil {
		t.Errorf("delete of the agent's skill: %v", err)
	}
	// without a booted.json (a bench, a test home) nothing is refused for it
	_ = os.Remove(filepath.Join(l.Home, BootedFile))
	if _, err := l.Delete("solo"); err != nil {
		t.Errorf("delete without booted.json: %v", err)
	}
}

// Delete takes the skill out of skills/ in one rename before removing its files, so a nap taken
// meanwhile never sees half a skill, and a skill whose files resist removal is still gone.
// What resists stays in the temp dir untouched until the boot sweep.
func TestDeleteIsOneStep(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	l := library(t)
	if _, err := l.Create("stuck", "", "---\nname: stuck\ndescription: d\n---\n"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.WriteFile("stuck", "data/a.md", "a"); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(l.Home, "skills", "stuck", "data")
	if err := os.Chmod(locked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = filepath.WalkDir(l.Home, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})
	if _, err := l.Delete("stuck"); err != nil {
		t.Errorf("delete: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(l.Home, "skills", "stuck")); !os.IsNotExist(err) {
		t.Errorf("skills/stuck is still there: %v", err)
	}
	// what resists removal stays in the temp dir as it is: never chmod-ed by path, which a
	// symlink raced in by the agent's commands would redirect (docs/needletail.md §7); the boot
	// sweep removes it by descriptor
	var left []string
	_ = filepath.WalkDir(filepath.Join(l.Home, TmpDir), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "data" {
			left = append(left, p)
			if info, err := os.Lstat(p); err != nil || info.Mode().Perm() != 0o555 {
				t.Errorf("%s was changed: %v %v", p, info.Mode(), err)
			}
		}
		return nil
	})
	if len(left) != 1 {
		t.Errorf("the locked directory is not left for the boot sweep: %v", left)
	}
	if err := fsx.Sweep(l.Home); err != nil {
		t.Errorf("sweep: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(l.Home, TmpDir)); len(entries) != 0 {
		t.Errorf("left in the temp dir after the sweep: %v", entries)
	}
}
