// Package sandbox confines the file tools' paths (docs/needletail.md §4.5, §6): every path must
// resolve, symlinks evaluated, inside the agent's home or one of its shared-space directories
// (/shared/group and /shared/<unit>), and inside the same one it names. It does not confine the
// terminal, scripts and monitors: the jail does (package jail, §7), with Landlock, when the kernel
// has it.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutside is a path that resolves outside every allowed root.
var ErrOutside = errors.New("outside the agent's home and shared space")

// Sandbox is the allowed roots. Relative paths are taken from Home.
type Sandbox struct {
	Home   string
	Shared []string
}

// Resolve is p as the file tools will open it: absolute, cleaned, symlinks of its existing part
// evaluated, and inside the root p names. The returned path is the one to use (never p itself),
// so a symlink cannot be swapped in between lexical and physical checks of the same string.
//
// A path stays in its own root: one under /shared/group must resolve under /shared/group, not
// into the home or the unit's directory through a symlink someone planted there (another agent
// can write the shared space), and one in the home must not leave it for the shared space.
func (s Sandbox) Resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("empty path")
	}
	if strings.ContainsRune(p, 0) {
		return "", errors.New("path contains NUL")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(s.Home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.Home, p)
	}
	p = filepath.Clean(p)
	root, ok := s.rootOf(p)
	if !ok {
		return "", fmt.Errorf("%s: %w", p, ErrOutside)
	}
	real, err := evalExisting(p)
	if err != nil {
		return "", err
	}
	if !within(real, root) {
		return "", fmt.Errorf("%s: %w", p, ErrOutside)
	}
	return real, nil
}

// rootOf is the root (resolved) that the cleaned absolute path p is lexically inside, by the
// root's configured spelling or its resolved one; the longest when roots nest.
func (s Sandbox) rootOf(p string) (string, bool) {
	best, n := "", -1
	for _, r := range append([]string{s.Home}, s.Shared...) {
		if r == "" {
			continue
		}
		clean := filepath.Clean(r)
		rr, err := evalExisting(clean)
		if err != nil {
			continue
		}
		for _, spelling := range []string{clean, rr} {
			if within(p, spelling) && len(spelling) > n {
				best, n = rr, len(spelling)
			}
		}
	}
	return best, n >= 0
}

// InShared: a resolved path inside the shared space (writes there are noted to the core).
func (s Sandbox) InShared(resolved string) bool {
	for _, r := range s.Shared {
		if rr, err := evalExisting(filepath.Clean(r)); err == nil && within(resolved, rr) {
			return true
		}
	}
	return false
}

// HomeRel is a resolved path relative to the home ("" and false when it is not inside it).
func (s Sandbox) HomeRel(resolved string) (string, bool) {
	home, err := evalExisting(filepath.Clean(s.Home))
	if err != nil || !within(resolved, home) {
		return "", false
	}
	rel, err := filepath.Rel(home, resolved)
	if err != nil {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// Below is a resolved path as its root (resolved) and the path below it (slash-separated), for
// writes that reach it by descriptor from the root (fsx.OpenFileBelow).
func (s Sandbox) Below(resolved string) (root, rel string, ok bool) {
	for _, r := range append([]string{s.Home}, s.Shared...) {
		if r == "" {
			continue
		}
		rr, err := evalExisting(filepath.Clean(r))
		if err != nil || resolved == rr || !within(resolved, rr) {
			continue
		}
		if len(rr) > len(root) {
			root = rr
		}
	}
	if root == "" {
		return "", "", false
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", "", false
	}
	return root, filepath.ToSlash(rel), true
}

// Split is a resolved path as its root (resolved) and the path below it, "" for a root itself:
// what a walk or a read by descriptor starts from (fsx.WalkBelow, fsx.OpenFileBelow).
func (s Sandbox) Split(resolved string) (root, rel string, ok bool) {
	if root, rel, ok := s.Below(resolved); ok {
		return root, rel, true
	}
	for _, r := range append([]string{s.Home}, s.Shared...) {
		if r == "" {
			continue
		}
		if rr, err := evalExisting(filepath.Clean(r)); err == nil && resolved == rr {
			return rr, "", true
		}
	}
	return "", "", false
}

// Display is how a resolved path is shown to the model: relative to the home when inside it.
func (s Sandbox) Display(resolved string) string {
	if rel, ok := s.HomeRel(resolved); ok {
		return rel
	}
	return resolved
}

func within(p, root string) bool {
	return p == root || strings.HasPrefix(p, strings.TrimSuffix(root, string(filepath.Separator))+string(filepath.Separator))
}

// evalExisting evaluates the symlinks of p's longest existing prefix and appends the rest. A
// dangling symlink is followed to where it points.
func evalExisting(p string) (string, error) { return evalDepth(p, 0) }

func evalDepth(p string, depth int) (string, error) {
	if depth > 40 {
		return "", errors.New("too many levels of symbolic links")
	}
	rest := []string{}
	cur := p
	for {
		if info, err := os.Lstat(cur); err == nil {
			real, err := filepath.EvalSymlinks(cur)
			if err != nil {
				if info.Mode()&os.ModeSymlink == 0 {
					return "", err
				}
				target, err := os.Readlink(cur)
				if err != nil {
					return "", err
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(cur), target)
				}
				for i := len(rest) - 1; i >= 0; i-- {
					target = filepath.Join(target, rest[i])
				}
				return evalDepth(filepath.Clean(target), depth+1)
			}
			for i := len(rest) - 1; i >= 0; i-- {
				real = filepath.Join(real, rest[i])
			}
			return real, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}
