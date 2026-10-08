# One agent's sidecar for ECS: the engine's sidecar image plus the instance files it needs. Run
# from the instance directory after `stormo build <agent>`:
#
#   docker build -f <engine>/docker/instance.Dockerfile --build-arg STORMO_IMAGE=stormo-sidecar:<version> \
#     --build-arg AGENT=<agent> --platform linux/arm64 -t <registry>/<names.resource>-<agent>:<tag> .
ARG STORMO_IMAGE
FROM ${STORMO_IMAGE}
COPY stormo.yaml ./
COPY units ./units
ARG AGENT
COPY agents/${AGENT}/agent.yaml agents/${AGENT}/SOUL.md ./agents/${AGENT}/
COPY dist/${AGENT}/baseline ./dist/${AGENT}/baseline
