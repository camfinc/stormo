package local

import (
	"fmt"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/manifest"
)

// Seams for tests: what EnsureEngineImage asks docker.
var (
	hasImage  = imageExists
	runDocker = run
)

// EnsureEngineImage makes the agent's engine image available under the name compose runs
// (Image): an engine that publishes its image elsewhere (engine.PublishedImage) gets a missing one
// pulled from there and tagged with that name. An image already here, a local build included, is
// used as it is. Other engines name a registry image, which compose pulls itself.
func EnsureEngineImage(eng engine.Engine, a *manifest.Agent, log func(string)) error {
	pub, ok := eng.(engine.PublishedImage)
	if !ok {
		return nil
	}
	local := eng.Image(a)
	if hasImage(local) {
		return nil
	}
	remote := pub.PublishedImage(a)
	log("pulling " + remote)
	if err := runDocker("docker", "pull", remote); err != nil {
		return fmt.Errorf("image %s is not here and %s could not be pulled (pin a published tag, or build it: docs/instances.md): %w", local, remote, err)
	}
	return runDocker("docker", "tag", remote, local)
}
