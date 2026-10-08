# Stormo sidecar: the engine binary that runs `stormo rehydrate` (once, before the agent) and
# `stormo nap --loop` (alongside it) on a task's shared volume. One image per engine version; an
# instance adds its files on top (docker/instance.Dockerfile for ECS, bind mounts locally).
#
#   docker build -f docker/sidecar.Dockerfile --build-arg VERSION=$(git rev-parse --short=12 HEAD) \
#     --platform linux/arm64 -t stormo-sidecar:<version> .
FROM golang:1.26.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY pkg ./pkg
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X github.com/camfinc/stormo/pkg/version.Version=${VERSION}" -o /out/stormo ./cmd/stormo

FROM alpine:3.22
COPY --from=build /out/stormo /usr/local/bin/stormo
WORKDIR /app
ENTRYPOINT ["stormo"]
