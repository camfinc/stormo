# The transcribe capability's worker: whisper.cpp's server (the model stays loaded while the worker
# is awake) behind a small adapter that speaks the capability contract (docs/capabilities.md).
FROM debian:bookworm-20261005-slim AS whisper
RUN apt-get update && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
ARG WHISPER_CPP=v1.9.5
RUN git clone --depth 1 --branch "$WHISPER_CPP" https://github.com/ggml-org/whisper.cpp /src \
 && cmake -S /src -B /build -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF -DGGML_NATIVE=OFF \
      -DWHISPER_BUILD_TESTS=OFF -DWHISPER_BUILD_EXAMPLES=ON -DWHISPER_SDL2=OFF \
 && cmake --build /build -j"$(nproc)" --target whisper-server \
 && cp /build/bin/whisper-server /usr/local/bin/whisper-server

FROM golang:1.26.8-bookworm AS adapter
WORKDIR /src
COPY server.go .
RUN CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/adapter server.go

FROM debian:bookworm-20261005-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates libgomp1 \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /cache && chmod 0777 /cache
COPY --from=whisper /usr/local/bin/whisper-server /usr/local/bin/whisper-server
COPY --from=adapter /usr/local/bin/adapter /usr/local/bin/adapter
ENV HOME=/tmp PORT=8080
EXPOSE 8080
CMD ["/usr/local/bin/adapter"]
