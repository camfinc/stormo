# The browser capability's worker: Playwright's image (Chromium and its system libraries), the
# matching playwright-core installed with bun from the lockfile, run under node.
FROM oven/bun:1.4.3 AS deps
WORKDIR /app
COPY package.json bun.lock bunfig.toml ./
RUN bun install --frozen-lockfile --production

FROM mcr.microsoft.com/playwright:v1.64.1-noble
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY package.json server.mjs ./
RUN mkdir -p /cache && chmod 0777 /cache
ENV HOME=/tmp PORT=8080 PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
EXPOSE 8080
CMD ["node", "server.mjs"]
