# Stormo sidecar: the engine binary that runs `stormo rehydrate` (once, before the agent) and
# `stormo nap --loop` (alongside it) on a task's shared volume. One image per engine version; an
# instance adds its files on top (docker/instance.Dockerfile for ECS, bind mounts locally).
#
# The Go stage runs on the build machine and cross-compiles for each target platform, so a
# multi-arch build needs no emulation:
#
#   docker buildx build -f docker/sidecar.Dockerfile --platform linux/amd64,linux/arm64 \
#     --build-arg VERSION=<version> -t stormo-sidecar:<version> .
FROM --platform=$BUILDPLATFORM golang:1.26.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY embed.go ./
COPY docs/instances.md ./docs/
COPY examples/minimal ./examples/minimal
COPY cmd ./cmd
COPY pkg ./pkg
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/camfinc/stormo/pkg/version.Version=${VERSION}" -o /out/stormo ./cmd/stormo

FROM alpine:3.22
COPY --from=build /out/stormo /usr/local/bin/stormo
WORKDIR /app
ENTRYPOINT ["stormo"]
