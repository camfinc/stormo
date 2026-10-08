// Package version names the engine build: a release tag set at link time, or, for a build from a
// source checkout, that checkout's git revision ("-dirty" with local changes), or "dev".
package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Version is set by release builds: -ldflags "-X github.com/camfinc/stormo/pkg/version.Version=v0.1.0".
var Version = ""

var (
	once sync.Once
	rev  string
)

// String is the version of this binary.
func String() string {
	if Version != "" {
		return Version
	}
	once.Do(func() { rev = sourceRevision() })
	return rev
}

// Released reports a tagged release (its sidecar image is published).
func Released() bool { return Version != "" }

// sourceRevision asks the engine checkout itself. Go's own VCS stamping is not used: inside a
// submodule it records the parent repository's commit.
func sourceRevision() string {
	src := Source()
	if src == "" {
		return "dev"
	}
	out, err := exec.Command("git", "-C", src, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return "dev"
	}
	r := strings.TrimSpace(string(out))
	if st, err := exec.Command("git", "-C", src, "status", "--porcelain", "--untracked-files=no").Output(); err == nil && len(strings.TrimSpace(string(st))) > 0 {
		r += "-dirty"
	}
	return r
}

// Source finds the engine's source checkout: STORMO_SRC, or the nearest directory above the binary
// holding the engine's go.mod. "" for an installed release.
func Source() string {
	if s := os.Getenv("STORMO_SRC"); s != "" {
		return s
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.Contains(string(b), "module github.com/camfinc/stormo\n") {
			return dir
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}
