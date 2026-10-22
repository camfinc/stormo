# Needletail: the in-container agent runtime (docs/needletail.md). One static Go binary
# (CGO_ENABLED=0) on Alpine with the tools skill scripts use from the terminal (bash, python3, git).
#
# The Go stage runs on the build machine and cross-compiles for each target platform, so a
# multi-arch build needs no emulation. Build from the repository root:
#
#   docker buildx build -f deploy/needletail/Dockerfile --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=<version> -t needletail:<version> .
#
# Base images are pinned by their multi-arch index digest; bump tag and digest together.
FROM --platform=$BUILDPLATFORM golang:1.26.2-alpine@sha256:f85330846cde1e57ca9ec309382da3b8e6ae3ab943d2739500e08c86393a21b1 AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal/needletail ./internal/needletail
COPY cmd/needletail ./cmd/needletail
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
# Only the binary's own modules are fetched (go.yaml.in/yaml/v3, github.com/gorilla/websocket,
# github.com/tetratelabs/wazero and its golang.org/x/sys), checked against go.sum. The Luau module
# (internal/needletail/luau/wasm/luau.wasm) is committed and embedded: no C toolchain is needed.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/camfinc/stormo/internal/needletail/cli.Version=${VERSION}" \
    -o /out/needletail ./cmd/needletail

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache bash python3 git ca-certificates \
    && mkdir -p /opt/data \
    && chown 10000:10000 /opt/data
COPY --from=build /out/needletail /usr/local/bin/needletail
ENV AGENT_HOME=/opt/data \
    NEEDLETAIL_API_PORT=8642
WORKDIR /opt/data
# No USER: the container starts as root and the runtime splits itself (docs/needletail.md §7).
# It lays the home out, then runs as 10000:10000 (shared.PosixUID, the home's and the shared
# space's owner) keeping only CAP_SETUID, CAP_SETGID and CAP_KILL, and starts everything the agent
# executes (the terminal, scheduled scripts and monitors) as 10001:10000 with no capabilities.
# Started as any other user it runs as that one user, without the split (logged).
EXPOSE 8642
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/usr/local/bin/needletail", "healthcheck"]
# The engine's Command is ["serve"]; the bench runs `needletail oneshot …` (the binary drops its
# own name when it comes first).
ENTRYPOINT ["/usr/local/bin/needletail"]
CMD ["serve"]
