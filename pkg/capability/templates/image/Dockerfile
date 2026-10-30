# The image capability's worker: a small adapter that speaks the capability contract
# (docs/capabilities.md) and asks OpenRouter's Image API for one picture per job.
FROM golang:1.26.8-bookworm AS adapter
WORKDIR /src
COPY server.go .
RUN CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/adapter server.go

FROM debian:bookworm-20261005-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /cache && chmod 0777 /cache
COPY --from=adapter /usr/local/bin/adapter /usr/local/bin/adapter
ENV HOME=/tmp PORT=8080
EXPOSE 8080
CMD ["/usr/local/bin/adapter"]
