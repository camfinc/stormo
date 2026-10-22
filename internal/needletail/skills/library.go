package skills

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/camfinc/stormo/internal/needletail/fsx"
)

// DirVar is the placeholder a skill's text uses for its own directory (Stormo form). The build
// leaves it as it is; the runtime resolves it when it shows a skill (View) and exports it as an
// env var when the terminal runs a command from a skill.
const DirVar = "SKILL_DIR"

// Bounds on what a skill holds and what one view returns.
const (
	MaxFile   = maxSkillFile // one file a skill_manage call writes
	viewBytes = 100_000      // one file's text in a view
	viewFiles = 200          // files a view lists
)

// Library is one home's skills: finding and showing them, the agent's own changes (manage.go) and
// their usage counts (usage.go). Safe for concurrent use.
type Library struct {
	Home string
	// Generated are skill dirs (relative to skills/) the build generates from the instance, such
	// as its knowledge and shared-docs skills: shown and used like any skill, never changed by
	// skill_manage, and never proposed by the dream (which skips them by name), so a change would
	// only be lost at the next build.
	Generated []string
	Now       func() time.Time
	// Check, when set, refuses text a skill file may not hold (the runtime's secret values): it
	// sees every file skill_manage would write as a whole, so text split across calls is caught.
	Check func(text string) error
	// CheckLuau, when set, validates a .luau file skill_manage would leave (rel is its path in the
	// skill, body the whole file: header and compile); without it .luau files are refused.
	CheckLuau func(rel string, body []byte) error
	// LuauTools, when set, is a skill's Luau tools as a view of its SKILL.md lists them (an invalid
	// one with its error).
	LuauTools func(dir string) []ToolStatus

	mu     sync.Mutex
	viewed map[string]string // session → dir of the skill it last viewed
}

// ErrAmbiguous is a name more than one skill answers to.
var ErrAmbiguous = errors.New("more than one skill answers to that name")

func (l *Library) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Library) root() string { return filepath.Join(l.Home, "skills") }

// TmpDir is where skill files are written before they are renamed into place (fsx.TmpDir).
const TmpDir = fsx.TmpDir

func (l *Library) tmp() string { return filepath.Join(l.Home, TmpDir) }

// Abs is a skill's directory as an absolute path: what ${SKILL_DIR} stands for.
func (l *Library) Abs(s Skill) string { return filepath.Join(l.root(), filepath.FromSlash(s.Dir)) }

// Index is every skill in the home.
func (l *Library) Index() []Skill { return Index(l.Home) }

// Find is the skill name names: its dir ("crm/crm-api", also as skills/crm/crm-api, its SKILL.md
// path or the absolute dir), else its front-matter name, else its directory's last segment.
func (l *Library) Find(name string) (Skill, error) {
	n := filepath.ToSlash(strings.TrimSpace(name))
	if abs := filepath.ToSlash(l.root()) + "/"; strings.HasPrefix(n, abs) {
		n = strings.TrimPrefix(n, abs)
	}
	n = strings.TrimPrefix(n, "./")
	n = strings.TrimPrefix(n, "skills/")
	n = strings.Trim(strings.TrimSuffix(n, "/SKILL.md"), "/")
	if n == "" {
		return Skill{}, errors.New("name is empty")
	}
	idx := l.Index()
	for _, key := range []func(Skill) string{
		func(s Skill) string { return s.Dir },
		func(s Skill) string { return s.Name },
		func(s Skill) string { return path.Base(s.Dir) },
	} {
		var found []Skill
		for _, s := range idx {
			if key(s) == n {
				found = append(found, s)
			}
		}
		switch len(found) {
		case 1:
			return found[0], nil
		case 0:
			continue
		}
		dirs := make([]string, len(found))
		for i, s := range found {
			dirs[i] = s.Dir
		}
		return Skill{}, fmt.Errorf("%q: %w (%s): give its dir", name, ErrAmbiguous, strings.Join(dirs, ", "))
	}
	return Skill{}, fmt.Errorf("no skill %q (skills_list lists them)", name)
}

// ReadOnly reports a skill dir the agent may not change: one the build generates (by its dir, or
// by its name anywhere, since the dream skips generated skills by name) or a hidden one.
func (l *Library) ReadOnly(dir string) bool {
	for _, seg := range strings.Split(dir, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	for _, g := range l.Generated {
		if below(dir, g) || strings.EqualFold(path.Base(dir), path.Base(g)) {
			return true
		}
	}
	return false
}

// below is whether dir is top or inside it, segment by segment as Unicode case folds
// (strings.EqualFold: ſ is s): on a case-insensitive home any spelling the filesystem folds
// together is the same directory.
func below(dir, top string) bool {
	d, t := strings.Split(dir, "/"), strings.Split(top, "/")
	if len(d) < len(t) {
		return false
	}
	for i := range t {
		if !strings.EqualFold(d[i], t[i]) {
			return false
		}
	}
	return true
}

// Resolve is text with ${SKILL_DIR} replaced by the skill's directory.
func (l *Library) Resolve(s Skill, text string) string {
	return strings.ReplaceAll(text, "${"+DirVar+"}", l.Abs(s))
}

// Unresolve is text with the skill's directory put back as ${SKILL_DIR} (what View resolved): the
// directory followed by "/", a quote, a space or the end, never a longer name that starts like it
// (crm-api2). Text that is not UTF-8, or holds a NUL, is left as it is.
func (l *Library) Unresolve(s Skill, text string) string {
	abs := l.Abs(s)
	if !strings.Contains(text, abs) || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return text
	}
	var b strings.Builder
	from := 0
	for {
		i := strings.Index(text[from:], abs)
		if i < 0 {
			break
		}
		i += from
		end := i + len(abs)
		if end < len(text) && nameByte(text[end]) {
			b.WriteString(text[from:end])
		} else {
			b.WriteString(text[from:i])
			b.WriteString("${" + DirVar + "}")
		}
		from = end
	}
	b.WriteString(text[from:])
	return b.String()
}

// cleanRel is a file path inside a skill, cleaned: relative, never leaving the skill, and with no
// hidden segment (which also refuses "..") or control character.
func cleanRel(file string) (string, error) {
	f := filepath.ToSlash(strings.TrimSpace(file))
	if f == "" {
		return "", errors.New("file_path is empty")
	}
	if strings.ContainsFunc(f, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", errors.New("file_path contains a control character")
	}
	if strings.HasPrefix(f, "/") {
		return "", fmt.Errorf("%s: give the path relative to the skill's directory", file)
	}
	for _, seg := range strings.Split(f, "/") {
		if strings.HasPrefix(seg, ".") && seg != "." {
			return "", fmt.Errorf("%s: hidden files and .. are not allowed in a skill", file)
		}
	}
	f = path.Clean(f)
	if f == "." {
		return "", fmt.Errorf("%s names the skill's directory, not a file", file)
	}
	return f, nil
}

// real is home/rel (rel slash-separated) when no existing component of it below the home is a
// symlink: a skill never leads out of the home, however its files were made.
func (l *Library) real(rel string) (string, error) {
	cur := l.Home
	for _, seg := range strings.Split(rel, "/") {
		cur = filepath.Join(cur, seg)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return filepath.Join(l.Home, filepath.FromSlash(rel)), nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", fmt.Errorf("%s is a symlink: skills may not contain links", strings.TrimPrefix(cur, l.Home+string(filepath.Separator)))
		}
	}
	return cur, nil
}

// readRegular reads p (a path in the home, from real) a regular file of at most limit bytes (a
// larger one is cut to limit and reported as such), by descriptor from the home: never through a
// symlink, non-blocking, checked a regular file on what was opened, so a FIFO or a link a
// command swaps in after real's check is refused at once.
func (l *Library) readRegular(p string, limit int) ([]byte, bool, error) {
	rel, err := filepath.Rel(l.Home, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, false, fmt.Errorf("%s is not in the home", p)
	}
	f, err := fsx.OpenFileBelow(l.Home, filepath.ToSlash(rel), os.O_RDONLY, 0, false)
	if errors.Is(err, fsx.ErrNotRegular) {
		return nil, false, fmt.Errorf("%s is not a regular file", filepath.Base(p))
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > limit {
		return b[:limit], true, nil
	}
	return b, false, nil
}

// View is what skill_view shows: a skill's SKILL.md (or one of its files) with ${SKILL_DIR}
// resolved, and the other files it holds, to load next.
type View struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Dir         string   `json:"dir"`
	SkillDir    string   `json:"skill_dir"`
	ReadOnly    bool     `json:"read_only,omitempty"`
	File        string   `json:"file"`
	Content     string   `json:"content"`
	Truncated   bool     `json:"truncated,omitempty"`
	Files       []string `json:"files,omitempty"`
	MoreFiles   bool     `json:"more_files,omitempty"`
	// Tools are the skill's Luau tools (tools/<name>.luau): registered, or why not.
	Tools []ToolStatus `json:"tools,omitempty"`
}

// ToolStatus is one Luau tool of a skill: its file, its registered name and declared and
// effective caps, or the error that keeps it from being registered.
type ToolStatus struct {
	File      string   `json:"file"`
	Name      string   `json:"name,omitempty"`
	Caps      []string `json:"caps,omitempty"`
	Effective []string `json:"effective_caps,omitempty"`
	// Events and Jobs are a handler's (handlers/<name>.luau): what it handles, and the jobs its
	// on_schedule serves.
	Events []string `json:"events,omitempty"`
	Jobs   []string `json:"jobs,omitempty"`
	Error  string   `json:"error,omitempty"`
}

// View shows the skill name names: its SKILL.md and file list, or one of its files (file, relative
// to the skill's directory). A SKILL.md view counts as a view of the skill (and as the session's
// current skill, for the terminal); a file view as a use.
func (l *Library) View(session, name, file string) (*View, error) {
	s, err := l.Find(name)
	if err != nil {
		return nil, err
	}
	rel := "SKILL.md"
	if strings.TrimSpace(file) != "" {
		if rel, err = cleanRel(file); err != nil {
			return nil, err
		}
	}
	p, err := l.real("skills/" + s.Dir + "/" + rel)
	if err != nil {
		return nil, err
	}
	body, cut, err := l.readRegular(p, viewBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s has no file %s", s.Dir, rel)
	}
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(body, 0) >= 0 || (!cut && !utf8.Valid(body)) {
		return nil, fmt.Errorf("%s/%s is binary: use the terminal with it", s.Dir, rel)
	}
	v := &View{Name: s.Name, Description: s.Description, Dir: s.Dir, SkillDir: l.Abs(s), ReadOnly: l.ReadOnly(s.Dir),
		File: rel, Content: l.Resolve(s, strings.ToValidUTF8(string(body), "")), Truncated: cut}
	if rel == "SKILL.md" {
		v.Files, v.MoreFiles = l.files(s)
		if l.LuauTools != nil {
			v.Tools = l.LuauTools(s.Dir)
		}
		l.Viewed(session, s)
	} else {
		l.Used(s)
	}
	return v, nil
}

// files is the skill's files other than SKILL.md, relative to its directory: regular files only,
// hidden ones and nested skills left out.
func (l *Library) files(s Skill) ([]string, bool) {
	dir, err := l.real("skills/" + s.Dir)
	if err != nil {
		return nil, false
	}
	out := []string{}
	more := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == dir {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if _, err := os.Lstat(filepath.Join(p, "SKILL.md")); err == nil {
				return filepath.SkipDir // a skill of its own
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() || rel == "SKILL.md" {
			return nil
		}
		if len(out) == viewFiles {
			more = true
			return filepath.SkipAll
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out, more
}

// Referenced is the one skill whose directory a terminal command names (as skills/<dir>/…,
// absolute or relative, skills/ at a path boundary): the skill the command runs from. When it
// names several, none.
func (l *Library) Referenced(command string) (Skill, bool) {
	var best Skill
	found := false
	for _, s := range l.Index() {
		if !containsAtBoundary(command, "skills/"+s.Dir+"/") {
			continue
		}
		switch {
		case !found:
			best, found = s, true
		case strings.HasPrefix(s.Dir, best.Dir+"/"):
			best = s // the deeper skill: its own files are not its parent's
		case strings.HasPrefix(best.Dir, s.Dir+"/"):
		default:
			return Skill{}, false
		}
	}
	return best, found
}

// containsAtBoundary reports whether s holds sub where the byte before it (if any) cannot be part
// of a path segment's name: "/x/skills/a/" and "skills/a/" hold "skills/a/", "/x/otherskills/a/"
// does not.
func containsAtBoundary(s, sub string) bool {
	for from := 0; ; {
		i := strings.Index(s[from:], sub)
		if i < 0 {
			return false
		}
		i += from
		if i == 0 || !nameByte(s[i-1]) {
			return true
		}
		from = i + 1
	}
}

// nameByte is a byte a file or directory name is usually made of.
func nameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.'
}

// LastViewed is the skill session last viewed with skill_view, while it still exists.
func (l *Library) LastViewed(session string) (Skill, bool) {
	l.mu.Lock()
	dir, ok := l.viewed[session]
	l.mu.Unlock()
	if !ok {
		return Skill{}, false
	}
	s, err := l.Find(dir)
	return s, err == nil && s.Dir == dir
}

// Moved carries the skill session from last viewed to session to, which continues it (a
// compaction's child). from keeps it too: a call still running under it finds it.
func (l *Library) Moved(from, to string) {
	l.mu.Lock()
	dir, ok := l.viewed[from]
	l.mu.Unlock()
	if ok {
		l.setViewed(to, dir)
	}
}

// maxSessions bounds the sessions whose last viewed skill is remembered.
const maxSessions = 256

func (l *Library) setViewed(session, dir string) {
	if session == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.viewed == nil {
		l.viewed = map[string]string{}
	}
	if _, ok := l.viewed[session]; !ok && len(l.viewed) >= maxSessions {
		for k := range l.viewed { // forget an arbitrary one: this is only a convenience
			delete(l.viewed, k)
			break
		}
	}
	l.viewed[session] = dir
}
