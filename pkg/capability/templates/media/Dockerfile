# The media capability's worker: ffmpeg behind a small adapter that speaks the capability contract
# (docs/capabilities.md): probe, QA, stills, loudness, mastering, cover frames.
FROM golang:1.26.8-bookworm AS adapter
WORKDIR /src
COPY server.go .
RUN CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/adapter server.go

FROM debian:bookworm-20261005-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg fonts-dejavu-core ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /cache && chmod 0777 /cache
COPY --from=adapter /usr/local/bin/adapter /usr/local/bin/adapter
ENV HOME=/tmp PORT=8080
EXPOSE 8080
CMD ["/usr/local/bin/adapter"]
