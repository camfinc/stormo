// Package sandbox confines the file tools' paths (docs/needletail.md §4.5, §6): every path must
// resolve, symlinks evaluated, inside the agent's home or one of its shared-space directories
// (/shared/group and /shared/<unit>). It does not confine the terminal, whose boundary is the
// container (§7).
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
// evaluated, and inside a root. The returned path is the one to use (never p itself), so a
// symlink cannot be swapped in between lexical and physical checks of the same string.
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
	real, err := evalExisting(filepath.Clean(p))
	if err != nil {
		return "", err
	}
	for _, root := range s.roots() {
		if within(real, root) {
			return real, nil
		}
	}
	return "", fmt.Errorf("%s: %w", p, ErrOutside)
}

// roots are the allowed roots, themselves resolved.
func (s Sandbox) roots() []string {
	out := []string{}
	for _, r := range append([]string{s.Home}, s.Shared...) {
		if r == "" {
			continue
		}
		if rr, err := evalExisting(filepath.Clean(r)); err == nil {
			out = append(out, rr)
		}
	}
	return out
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
