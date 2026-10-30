# The video capability's worker: Remotion (installed with bun from the lockfile, run under node),
# Chrome Headless Shell and its system libraries, and ffmpeg for mastering and stills.
FROM oven/bun:1.4.3 AS deps
WORKDIR /app
COPY package.json bun.lock bunfig.toml ./
RUN bun install --frozen-lockfile --production

FROM node:22.23.3-bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates fonts-noto-core fonts-noto-color-emoji \
      libnss3 libdbus-1-3 libatk1.0-0 libatk-bridge2.0-0 libgbm1 libasound2 libxrandr2 libxkbcommon0 libxfixes3 \
      libxcomposite1 libxdamage1 libpango-1.0-0 libcairo2 libcups2 \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /cache && chmod 0777 /cache
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY package.json server.mjs ./
# Chrome Headless Shell goes into node_modules/.remotion, readable by whichever user runs the worker.
RUN node -e "require('@remotion/renderer').ensureBrowser().then(() => process.exit(0))" \
 && chmod -R a+rX /app/node_modules
ENV HOME=/tmp PORT=8080
EXPOSE 8080
CMD ["node", "server.mjs"]
