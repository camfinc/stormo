# Build from the repository root. Export this filesystem; it is not a Docker daemon image.
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates git iproute2 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --shell /bin/bash stormo \
    && mkdir -p /home/stormo/Instances \
    && chown stormo:stormo /home/stormo/Instances \
    && printf '[user]\ndefault=stormo\n' > /etc/wsl.conf \
    && printf 'stormo-desktop-runtime-v1\n' > /etc/stormo-runtime
# An archived source tree allows an unsigned development preview to build its own sidecar.
# Tagged production bundles must pair their CLI version with a published sidecar image.
COPY . /opt/stormo/
COPY apps/windows/build/stormo /opt/stormo/bin/stormo
RUN chmod 755 /opt/stormo/bin/stormo
