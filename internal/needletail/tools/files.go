package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/sandbox"
)

const (
	readDefaultLines = 500
	readMaxLines     = 2000
	readMaxBytes     = 100_000
	lineMaxBytes     = 2000
	searchDefault    = 50
	searchMax        = 500
	searchFileMax    = 2 << 20
	searchScanMax    = 20_000 // files visited per search
	patchMaxBytes    = 8 << 20
)

// protected are home paths the file tools never write: the runtime's own files and what the
// managing tools keep (cron/ is cronjob_manage's: its bounds and the messaging policy for an
// agent job's targets; .swarm/ holds booted.json, by which skill_manage refuses to delete a repo
// skill; logs/ holds the Luau audit). The terminal can still reach them until the runtime and the
// agent's commands run as different users (M6b); the container is the boundary, §7.
var protected = []string{"sessions", "state", "needletail.yaml", "cron", "logs", ".swarm"}

// skipDirs are never searched: the runtime's files, VCS and dependency trees.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "__pycache__": true, ".venv": true}

// isProtected compares case-folded: on a case-insensitive home Cron/ is cron/.
func isProtected(rel string) bool {
	rel = strings.ToLower(rel)
	for _, p := range protected {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// writable resolves p for a write: inside the sandbox and not a protected runtime file.
func writable(env *Env, p string) (string, error) {
	real, err := env.Sandbox.Resolve(p)
	if err != nil {
		return "", err
	}
	if rel, ok := env.Sandbox.HomeRel(real); ok && isProtected(rel) {
		return "", fmt.Errorf("%s is the runtime's own file and cannot be written with the file tools", rel)
	}
	if info, err := os.Stat(real); err == nil && info.IsDir() {
		return "", fmt.Errorf("%s is a directory", env.Sandbox.Display(real))
	}
	return real, nil
}

// openRead opens a resolved path for reading: a regular file only (a FIFO would block the call,
// and the turn with it, until a writer came), never through a symlink swapped in since Resolve.
func openRead(real, display string) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(real)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", display, unwrapPath(err))
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("%s is a directory: use search_files with target files to list it", display)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", display)
	}
	f, err := os.OpenFile(real, os.O_RDONLY|oNoFollow|oNonblock, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", display, unwrapPath(err))
	}
	got, err := f.Stat()
	if err != nil || !got.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file", display)
	}
	return f, got, nil
}

// writeFile replaces the content of the resolved path real (created with mode when missing): a
// regular file only, opened without following a final symlink, and only when the path still
// resolves to itself and names the file opened (nothing was swapped in since Resolve). The file
// is truncated only after those checks.
func writeFile(env *Env, real string, body []byte, mode os.FileMode) error {
	display := env.Sandbox.Display(real)
	if info, err := os.Lstat(real); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", display)
	}
	// The file is reached by descriptor from its root (the home or a shared layer), never
	// through a symlink, its directories made on the way: a directory the agent's commands
	// swap for a link after the checks cannot send the write to the runtime's own files.
	root, rel, ok := env.Sandbox.Below(real)
	if !ok {
		return fmt.Errorf("%s: %w", display, sandbox.ErrOutside)
	}
	f, err := fsx.OpenFileBelow(root, rel, os.O_WRONLY|os.O_CREATE, mode, true)
	if err != nil {
		return fmt.Errorf("%s: %w", display, unwrapPath(err))
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	again, err := env.Sandbox.Resolve(real)
	now, serr := os.Lstat(real)
	if err != nil || again != real || serr != nil || !os.SameFile(opened, now) {
		return fmt.Errorf("%s moved while writing", display)
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		return err
	}
	return f.Close()
}

// noteSkillRead counts a read of a skill's SKILL.md with read_file as a view of it (skill_view
// is the tool for it, but a model may use either).
func noteSkillRead(env *Env, real string) {
	if env.Skills == nil || filepath.Base(real) != "SKILL.md" {
		return
	}
	rel, ok := env.Sandbox.HomeRel(real)
	if !ok || !strings.HasPrefix(rel, "skills/") {
		return
	}
	if s, err := env.Skills.Find(rel); err == nil && s.Path == rel {
		env.Skills.Viewed(env.Session, s)
	}
}

func noteShared(ctx context.Context, env *Env, real string) {
	if env.SharedWrite != nil && env.Sandbox.InShared(real) {
		env.SharedWrite(ctx, real)
	}
}

// ---- read_file -------------------------------------------------------------------------------

func readFileTool() Tool {
	return Tool{
		Name: "read_file",
		Description: "Read a text file from the agent's home (relative paths) or its shared space (/shared/group, " +
			"/shared/<unit>). Lines come numbered as LINE|text; use offset and limit to page through long files.",
		Schema: schema(`{"type":"object","properties":{
"path":{"type":"string","description":"File path, relative to the home or absolute."},
"offset":{"type":"integer","description":"First line to return, from 1 (default 1)."},
"limit":{"type":"integer","description":"Lines to return (default 500, at most 2000)."}},
"required":["path"]}`),
		Run: func(_ context.Context, env *Env, raw json.RawMessage) (any, error) {
			var a struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			real, err := env.Sandbox.Resolve(a.Path)
			if err != nil {
				return nil, err
			}
			out, err := readLines(real, env.Sandbox.Display(real), a.Offset, a.Limit)
			if err == nil {
				noteSkillRead(env, real)
			}
			return out, err
		},
	}
}

type readResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	TotalLines int    `json:"total_lines"`
	Truncated  bool   `json:"truncated,omitempty"`
	Next       int    `json:"next_offset,omitempty"`
}

func readLines(real, display string, offset, limit int) (any, error) {
	f, _, err := openRead(real, display)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 8192)
	n, _ := io.ReadFull(f, head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, fmt.Errorf("%s looks binary", display)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = readDefaultLines
	}
	limit = min(limit, readMaxLines)
	res := readResult{Path: display}
	var b strings.Builder
	br := bufio.NewReaderSize(f, 64<<10)
	line := 0
	var buf []byte
	for {
		// at most lineMaxBytes and a line ending of each line is held, however long it is
		kept, raw, err := readLine(br, lineMaxBytes+2, buf)
		buf = kept
		if raw == 0 && err != nil {
			break
		}
		line++
		if line >= offset && line < offset+limit && !res.Truncated {
			s := string(kept)
			if raw > len(kept) {
				s = s[:lineMaxBytes] + "…"
			} else if s = strings.TrimRight(s, "\r\n"); len(s) > lineMaxBytes {
				s = s[:lineMaxBytes] + "…"
			}
			entry := fmt.Sprintf("%6d|%s\n", line, strings.ToValidUTF8(s, "�"))
			if b.Len()+len(entry) > readMaxBytes {
				res.Truncated, res.Next = true, line
			} else {
				b.WriteString(entry)
			}
		} else if line == offset+limit && !res.Truncated {
			res.Truncated, res.Next = true, line
		}
		if err != nil {
			break
		}
	}
	res.Content, res.TotalLines = b.String(), line
	return res, nil
}

// readLine reads one line (up to and including '\n', or to the end) keeping its first keep
// bytes in buf: the kept bytes, the line's full length and the read error (io.EOF at the end).
func readLine(br *bufio.Reader, keep int, buf []byte) ([]byte, int, error) {
	buf = buf[:0]
	raw := 0
	for {
		frag, err := br.ReadSlice('\n')
		if room := keep - len(buf); room > 0 {
			buf = append(buf, frag[:min(room, len(frag))]...)
		}
		raw += len(frag)
		if err == bufio.ErrBufferFull {
			continue
		}
		return buf, raw, err
	}
}

func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// ---- write_file ------------------------------------------------------------------------------

func writeFileTool() Tool {
	return Tool{
		Name: "write_file",
		Description: "Create or overwrite a text file in the agent's home or shared space with the given content " +
			"(parent directories are created). Use patch to change part of an existing file.",
		Schema: schema(`{"type":"object","properties":{
"path":{"type":"string","description":"File path, relative to the home or absolute."},
"content":{"type":"string","description":"The file's complete new content."}},
"required":["path","content"]}`),
		Run: func(ctx context.Context, env *Env, raw json.RawMessage) (any, error) {
			var a struct {
				Path    string  `json:"path"`
				Content *string `json:"content"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			if a.Content == nil {
				return nil, errors.New("content is required (use an empty string for an empty file)")
			}
			real, err := writable(env, a.Path)
			if err != nil {
				return nil, err
			}
			mode := os.FileMode(fsx.FilePerm)
			if info, err := os.Stat(real); err == nil {
				mode = info.Mode().Perm()
			}
			if err := writeFile(env, real, []byte(*a.Content), mode); err != nil {
				return nil, err
			}
			noteShared(ctx, env, real)
			return map[string]any{"path": env.Sandbox.Display(real), "bytes_written": len(*a.Content)}, nil
		},
	}
}

// ---- patch -----------------------------------------------------------------------------------

func patchTool() Tool {
	return Tool{
		Name: "patch",
		Description: "Edit a text file by replacing old_string with new_string. old_string must match exactly " +
			"(whitespace included) and only once, unless replace_all is true.",
		Schema: schema(`{"type":"object","properties":{
"path":{"type":"string","description":"File path, relative to the home or absolute."},
"old_string":{"type":"string","description":"The exact text to replace."},
"new_string":{"type":"string","description":"The text to put in its place."},
"replace_all":{"type":"boolean","description":"Replace every occurrence (default false)."}},
"required":["path","old_string","new_string"]}`),
		Run: func(ctx context.Context, env *Env, raw json.RawMessage) (any, error) {
			var a struct {
				Path       string `json:"path"`
				Old        string `json:"old_string"`
				New        string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := decode(raw, &a); err != nil {
				return nil, err
			}
			if a.Old == "" {
				return nil, errors.New("old_string is empty: use write_file to create a file")
			}
			real, err := writable(env, a.Path)
			if err != nil {
				return nil, err
			}
			display := env.Sandbox.Display(real)
			f, info, err := openRead(real, display)
			if err != nil {
				return nil, err
			}
			tooLarge := fmt.Errorf("%s is too large to patch (%d bytes, at most %d): use the terminal", display, info.Size(), patchMaxBytes)
			if info.Size() > patchMaxBytes {
				_ = f.Close()
				return nil, tooLarge
			}
			body, err := io.ReadAll(io.LimitReader(f, patchMaxBytes+1))
			_ = f.Close()
			if err != nil {
				return nil, unwrapPath(err)
			}
			if len(body) > patchMaxBytes {
				return nil, tooLarge
			}
			s := string(body)
			n := strings.Count(s, a.Old)
			switch {
			case n == 0:
				return nil, fmt.Errorf("old_string not found in %s", env.Sandbox.Display(real))
			case n > 1 && !a.ReplaceAll:
				return nil, fmt.Errorf("old_string occurs %d times in %s: give more context or set replace_all", n, env.Sandbox.Display(real))
			}
			if a.ReplaceAll {
				s = strings.ReplaceAll(s, a.Old, a.New)
			} else {
				s = strings.Replace(s, a.Old, a.New, 1)
			}
			if err := writeFile(env, real, []byte(s), info.Mode().Perm()); err != nil {
				return nil, err
			}
			noteShared(ctx, env, real)
			if !a.ReplaceAll {
				n = 1
			}
			return map[string]any{"path": env.Sandbox.Display(real), "replacements": n}, nil
		},
	}
}

// ---- search_files ----------------------------------------------------------------------------

func searchFilesTool() Tool {
	return Tool{
		Name: "search_files",
		Description: "Search the agent's home or shared space. target \"content\" (default) finds lines matching a " +
			"regular expression; target \"files\" lists files whose name matches a glob (or contains the pattern). " +
			"Use it to list a directory too (target files, pattern *).",
		Schema: schema(`{"type":"object","properties":{
"pattern":{"type":"string","description":"Regular expression (content) or file-name glob (files)."},
"target":{"type":"string","enum":["content","files"],"description":"What to search (default content)."},
"path":{"type":"string","description":"Directory to search (default: the home)."},
"file_glob":{"type":"string","description":"Only search files whose name matches this glob, e.g. *.md (content only)."},
"limit":{"type":"integer","description":"Most results to return (default 50, at most 500)."}},
"required":["pattern"]}`),
		Run: runSearch,
	}
}

type searchArgs struct {
	Pattern  string `json:"pattern"`
	Target   string `json:"target"`
	Path     string `json:"path"`
	FileGlob string `json:"file_glob"`
	Limit    int    `json:"limit"`
}

func runSearch(ctx context.Context, env *Env, raw json.RawMessage) (any, error) {
	var a searchArgs
	if err := decode(raw, &a); err != nil {
		return nil, err
	}
	if a.Pattern == "" {
		return nil, errors.New("pattern is empty")
	}
	if a.Path == "" {
		a.Path = env.Home
	}
	if a.Target == "" {
		a.Target = "content"
	}
	if a.Limit <= 0 {
		a.Limit = searchDefault
	}
	a.Limit = min(a.Limit, searchMax)
	root, err := env.Sandbox.Resolve(a.Path)
	if err != nil {
		return nil, err
	}
	if runtimeDir(env, root) {
		return nil, fmt.Errorf("%s is the runtime's own directory and is not searched", env.Sandbox.Display(root))
	}
	var re *regexp.Regexp
	switch a.Target {
	case "content":
		if re, err = regexp.Compile(a.Pattern); err != nil {
			return nil, fmt.Errorf("pattern: %v", err)
		}
	case "files":
		if _, err := filepath.Match(a.Pattern, "x"); err != nil {
			return nil, fmt.Errorf("pattern: %v", err)
		}
	default:
		return nil, errors.New(`target must be "content" or "files"`)
	}
	results := []string{}
	truncated := false
	visited := 0
	stop := errors.New("stop")
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: skipped
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if p != root && (skipDirs[d.Name()] || runtimeDir(env, p)) {
				return filepath.SkipDir
			}
			return nil
		}
		if visited++; visited > searchScanMax {
			truncated = true
			return stop
		}
		if !d.Type().IsRegular() {
			return nil
		}
		show := env.Sandbox.Display(p)
		if a.Target == "files" {
			if nameMatch(a.Pattern, d.Name()) {
				results = append(results, show)
			}
		} else if a.FileGlob == "" || nameMatch(a.FileGlob, d.Name()) {
			grep(p, show, re, &results, a.Limit+1)
		}
		if len(results) > a.Limit {
			truncated = true
			return stop
		}
		return nil
	})
	if err != nil && err != stop {
		return nil, err
	}
	if len(results) > a.Limit {
		results = results[:a.Limit]
	}
	return map[string]any{"target": a.Target, "matches": results, "count": len(results), "truncated": truncated}, nil
}

// runtimeDirs are the runtime's own directories in the home, never searched (sessions hold whole
// conversations).
var runtimeDirs = []string{"sessions", "state", "logs"}

// runtimeDir: p is one of runtimeDirs or inside one.
func runtimeDir(env *Env, p string) bool {
	rel, ok := env.Sandbox.HomeRel(p)
	if !ok {
		return false
	}
	rel = strings.ToLower(rel)
	for _, d := range runtimeDirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

func nameMatch(pattern, name string) bool {
	if !strings.ContainsAny(pattern, "*?[") {
		return strings.Contains(strings.ToLower(name), strings.ToLower(pattern))
	}
	ok, _ := filepath.Match(pattern, name)
	return ok
}

func grep(p, show string, re *regexp.Regexp, results *[]string, max int) {
	f, info, err := openRead(p, show)
	if err != nil {
		return
	}
	defer f.Close()
	if info.Size() > searchFileMax {
		return
	}
	body, err := io.ReadAll(f)
	if err != nil || bytes.IndexByte(body[:min(len(body), 8192)], 0) >= 0 {
		return
	}
	for i, line := range strings.Split(string(body), "\n") {
		if re.MatchString(line) {
			line = strings.TrimSpace(line)
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			*results = append(*results, fmt.Sprintf("%s:%d: %s", show, i+1, strings.ToValidUTF8(line, "�")))
			if len(*results) >= max {
				return
			}
		}
	}
}
