# The device gateway (docs/devices.md): one stdlib Go binary on an empty image. It runs read-only,
# as a non-root user, with no Linux capabilities; the core hands it its key through the environment.
FROM golang:1.26.8-bookworm AS build
WORKDIR /src
COPY server.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /gateway server.go

FROM scratch
COPY --from=build /gateway /gateway
USER 65532:65532
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/gateway"]
