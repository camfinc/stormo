# The hyperframes capability's worker: the HyperFrames CLI (HTML + GSAP compositions rendered to
# video), installed with bun from the lockfile, on Playwright's image (Node, Chromium's headless
# shell and its libraries; shared layers with the browser worker) plus ffmpeg.
FROM oven/bun:1.4.3 AS deps
WORKDIR /app
COPY package.json bun.lock bunfig.toml ./
RUN bun install --frozen-lockfile --production

FROM mcr.microsoft.com/playwright:v1.64.1-noble
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg \
 && rm -rf /var/lib/apt/lists/* \
 && ln -s "$(ls -d /ms-playwright/chromium_headless_shell-*/chrome-headless-shell-*/chrome-headless-shell | head -1)" /usr/local/bin/chrome-headless-shell \
 && mkdir -p /cache && chmod 0777 /cache
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY package.json server.mjs ./
RUN chmod -R a+rX /app/node_modules
ENV HOME=/tmp PORT=8080 HYPERFRAMES_BROWSER_PATH=/usr/local/bin/chrome-headless-shell \
    HYPERFRAMES_NO_TELEMETRY=1 HYPERFRAMES_NO_UPDATE_CHECK=1 HYPERFRAMES_SKIP_SKILLS=1
EXPOSE 8080
CMD ["node", "server.mjs"]
