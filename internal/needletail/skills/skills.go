// Package skills is the agent's skills (skills/**/SKILL.md, Stormo form; docs/needletail.md §4.6):
// the index the system prompt lists, a Library that finds and shows one (skill_view, with
// ${SKILL_DIR} resolved), changes the agent's own (skill_manage) and counts their use in
// skills/.usage.json, which the nap keeps and the dream copies next to the learnings.
package skills

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// Skill is one SKILL.md's front matter.
type Skill struct {
	Name        string
	Description string
	// Path is the SKILL.md, relative to the home.
	Path string
	// Dir is the skill's directory relative to skills/ ("crm/crm-api"): what the build's skill
	// hashes and the dream's proposals name a skill by.
	Dir string
}

const maxSkillFile = 1 << 20

// Bounds on what the index takes from a SKILL.md (the system prompt lists every skill each turn,
// so one skill must never be able to fill the context).
const (
	// MaxDescription is the longest description a skill_manage write may give (in characters);
	// a longer one already on disk is cut to it, with an ellipsis, in the index.
	MaxDescription = 1024
	// maxIndexName and maxIndexDir bound a name and a dir as the index section shows them.
	maxIndexName = 64
	maxIndexDir  = 128
	// PromptIndexBytes bounds the system prompt's skills section.
	PromptIndexBytes = 20_000
)

// skipDirs are dependency directories a skill's scripts may bring along: never skills, and too
// many files to walk every turn.
var skipDirs = map[string]bool{"node_modules": true, "__pycache__": true, "site-packages": true}

// Index is every skill under home/skills, sorted by path. Hidden and dependency directories are
// skipped; a SKILL.md without a name is listed by its directory. Only the front matter is read.
func Index(home string) []Skill {
	root := filepath.Join(home, "skills")
	out := []Skill{}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()]) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "SKILL.md" || !d.Type().IsRegular() || filepath.Dir(p) == root {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxSkillFile {
			return nil
		}
		rel, _ := filepath.Rel(home, p)
		head, err := readFront(home, filepath.ToSlash(rel))
		if err != nil {
			return nil
		}
		s := front(head)
		s.Path = filepath.ToSlash(rel)
		s.Dir = strings.TrimSuffix(strings.TrimPrefix(s.Path, "skills/"), "/SKILL.md")
		if s.Name == "" {
			s.Name = filepath.Base(filepath.Dir(p))
		}
		out = append(out, s)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// readFront reads a SKILL.md up to the end of its front matter (the first "\n---" after the
// opening "---"), at most maxSkillFile bytes: all front() looks at, without the body. It is read
// by descriptor (rel below home, never through a symlink, a regular file only, non-blocking): a
// FIFO a command swaps in would otherwise block every turn's index.
func readFront(home, rel string) ([]byte, error) {
	f, err := fsx.OpenFileBelow(home, rel, os.O_RDONLY, 0, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 4096)
	for len(buf) < maxSkillFile {
		n, err := f.Read(chunk[:min(len(chunk), maxSkillFile-len(buf))])
		buf = append(buf, chunk[:n]...)
		if len(buf) >= 3 {
			b := bytes.TrimPrefix(buf, []byte("\xef\xbb\xbf"))
			if len(b) >= 3 && !bytes.HasPrefix(b, []byte("---")) {
				return buf, nil // no front matter
			}
			if len(b) > 3 && bytes.Contains(b[3:], []byte("\n---")) {
				return buf, nil
			}
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// front reads the YAML front matter's name and description, each on one line with no control
// characters, the description cut to MaxDescription characters.
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
	return Skill{Name: oneLine(fm.Name), Description: cut(oneLine(fm.Description), MaxDescription)}
}

// oneLine is s with its whitespace and control characters collapsed into single spaces.
func oneLine(s string) string {
	return strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == utf8.RuneError
	}), " ")
}

// cut is s at most n characters long, an ellipsis marking a cut.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i, c := 0, 0
	for i = range s {
		if c == n {
			break
		}
		c++
	}
	return strings.TrimRight(s[:i], " ") + "…"
}

// PromptIndex is the system prompt's list of skills ("- name: description (dir)" per line), at
// most maxBytes long: names and dirs are cut, and the skills that do not fit are counted in a last
// line pointing to skills_list.
func PromptIndex(idx []Skill, maxBytes int) string {
	var b strings.Builder
	for i, s := range idx {
		line := fmt.Sprintf("- %s: %s (%s)\n", cut(oneLine(s.Name), maxIndexName), s.Description, cut(oneLine(s.Dir), maxIndexDir))
		more := fmt.Sprintf("…and %d more (skills_list)\n", len(idx)-i)
		last := i == len(idx)-1
		if b.Len()+len(line) > maxBytes || (!last && b.Len()+len(line)+len(more) > maxBytes) {
			if b.Len()+len(more) <= maxBytes {
				b.WriteString(more)
			}
			break
		}
		b.WriteString(line)
	}
	return b.String()
}
