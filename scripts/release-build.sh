#!/usr/bin/env sh
# Cross-compiles the stormo binary for every released platform into dist/:
#   dist/stormo_<version>_<os>_<arch>.tar.gz   (stormo, README.md, LICENSE, NOTICE)
#   dist/SHA256SUMS
# Used by .github/workflows/release.yml; runs the same locally: scripts/release-build.sh v0.1.0
set -eu
VERSION="${1:?usage: scripts/release-build.sh <version, e.g. v0.1.0>}"
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"
rm -rf dist && mkdir -p dist
for p in $PLATFORMS; do
  os="${p%/*}"; arch="${p#*/}"
  name="stormo_${VERSION}_${os}_${arch}"
  mkdir -p "dist/$name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X github.com/camfinc/stormo/pkg/version.Version=${VERSION}" \
    -o "dist/$name/stormo" ./cmd/stormo
  cp README.md LICENSE NOTICE "dist/$name/"
  tar -C dist -czf "dist/$name.tar.gz" "$name"
  rm -rf "dist/$name"
  echo "built dist/$name.tar.gz"
done
( cd dist && if command -v sha256sum >/dev/null; then sha256sum ./*.tar.gz; else shasum -a 256 ./*.tar.gz; fi | sed 's# \./# #' > SHA256SUMS )
cat dist/SHA256SUMS
