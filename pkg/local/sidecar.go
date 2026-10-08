package local

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/camfinc/stormo/pkg/version"
)

// Published is where release sidecar images live.
const Published = "ghcr.io/camfinc/stormo-sidecar"

// SidecarImage is the local tag of this engine's sidecar image.
func SidecarImage() string { return "stormo-sidecar:" + version.String() }

func imageExists(tag string) bool {
	return exec.Command("docker", "image", "inspect", tag).Run() == nil
}

// EnsureSidecar makes the sidecar image for this engine version available: built from source for a
// source build (always rebuilt when the checkout is dirty or rebuild is set; Docker's cache keeps
// that quick), pulled for a release.
func EnsureSidecar(rebuild bool, log func(string)) (string, error) {
	tag := SidecarImage()
	dirty := strings.HasSuffix(tag, "-dirty") || strings.HasSuffix(tag, ":dev")
	if imageExists(tag) && !rebuild && !dirty {
		return tag, nil
	}
	if version.Released() {
		remote := Published + ":" + version.String()
		log("pulling " + remote)
		if err := run("docker", "pull", remote); err != nil {
			return "", err
		}
		return tag, run("docker", "tag", remote, tag)
	}
	src := version.Source()
	if src == "" {
		return "", fmt.Errorf("sidecar image %s is missing and the engine source is unknown: set STORMO_SRC to the stormo checkout", tag)
	}
	log("building " + tag + " from " + src)
	return tag, run("docker", "build", "-q", "-f", filepath.Join(src, "docker", "sidecar.Dockerfile"),
		"--build-arg", "VERSION="+version.String(), "-t", tag, src)
}

func run(argv ...string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
	}
	return nil
}
