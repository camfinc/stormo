// Package skills reads the agent's skills (skills/**/SKILL.md, Stormo form). M1 has the index the
// system prompt lists; skill_view, skill_manage, ${SKILL_DIR} and usage counts come with M3.
package skills

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Skill is one SKILL.md's front matter.
type Skill struct {
	Name        string
	Description string
	// Path is the SKILL.md, relative to the home.
	Path string
}

const maxSkillFile = 1 << 20

// Index is every skill under home/skills, sorted by path. Hidden directories are skipped; a
// SKILL.md without a name is listed by its directory.
func Index(home string) []Skill {
	root := filepath.Join(home, "skills")
	out := []Skill{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "SKILL.md" || !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxSkillFile {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(home, p)
		s := front(body)
		s.Path = filepath.ToSlash(rel)
		if s.Name == "" {
			s.Name = filepath.Base(filepath.Dir(p))
		}
		out = append(out, s)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// front reads the YAML front matter's name and description.
func front(body []byte) Skill {
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	if !bytes.HasPrefix(body, []byte("---")) {
		return Skill{}
	}
	rest := body[3:]
	end := bytes.Index(rest, []byte("\n---"))
	if end < 0 {
		return Skill{}
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if yaml.Unmarshal(rest[:end], &fm) != nil {
		return Skill{}
	}
	return Skill{Name: strings.TrimSpace(fm.Name), Description: strings.Join(strings.Fields(fm.Description), " ")}
}
