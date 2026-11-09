#!/usr/bin/env sh
# Builds the needletail image for linux/amd64 and linux/arm64 and pushes it:
#   scripts/needletail-image.sh <tag> <version>    e.g. v0.5.2 0.5.2, or dev-abc1234 0.1.0-dev.abc1234
# IMAGE names the repository (default ghcr.io/camfinc/needletail, engine.PublishedImage's), and the
# caller is logged in to its registry. Used by .github/workflows/release.yml and needletail-image.yml.
# The Go stage cross-compiles, but the final stage runs apk on each platform, so arm64 needs QEMU:
# the pinned binfmt image registers it (a no-op where it is already registered).
set -eu
TAG="${1:?usage: scripts/needletail-image.sh <tag> <version>}"
VERSION="${2:?usage: scripts/needletail-image.sh <tag> <version>}"
IMAGE="${IMAGE:-ghcr.io/camfinc/needletail}"
SOURCE="https://github.com/${GITHUB_REPOSITORY:-camfinc/stormo}"
docker run --privileged --rm \
  docker.io/tonistiigi/binfmt:qemu-v10.2.3@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0 \
  --install arm64
docker buildx inspect needletail >/dev/null 2>&1 || docker buildx create --name needletail >/dev/null
docker buildx build --builder needletail --push -f deploy/needletail/Dockerfile \
  --platform linux/amd64,linux/arm64 \
  --build-arg VERSION="$VERSION" \
  --label org.opencontainers.image.source="$SOURCE" \
  --label org.opencontainers.image.version="$VERSION" \
  -t "$IMAGE:$TAG" .
echo "pushed $IMAGE:$TAG"
