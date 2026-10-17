# syntax=docker/dockerfile:1
# Build the frontend and binary from this checkout; no published image is required.
FROM --platform=$BUILDPLATFORM node:22-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package*.json ./
RUN npm ci
COPY web/ ./
COPY locale/definitions.json /src/locale/definitions.json
RUN npm run build:static

FROM --platform=$BUILDPLATFORM golang:1.26.3-bookworm AS backend
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=frontend /src/web/out/ ./server/webui/dist/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -tags embedui -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" -o /scopeweaver ./cmd/artex

FROM python:3.12-slim-bookworm
# Runtime tools and Node 22 for Playwright and camoufox-js (requires Node >= 22). Browser installation occurs at build time.
# Each npm step cleans its cache: it would otherwise stay in the layer and double its size.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates ripgrep curl wget vim git jq unzip \
      dnsutils iputils-ping netcat-openbsd inetutils-telnet whois nmap \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*
RUN npm install -g @playwright/mcp@0.0.83 @playwright/cli@latest playwright@latest \
    && playwright-cli --help \
    && playwright install --with-deps chromium \
    && npm cache clean --force && rm -rf /var/lib/apt/lists/*
# Camoufox (anti-detect Firefox) backs the built-in browser MCP. It runs headed under Xvfb.
# --ignore-scripts keeps native deps on their bundled prebuilds (no compiler in this image).
# wrap.mjs is copied separately so editing it does not invalidate the browser download layer.
COPY tools/camoufox-mcp/package.json tools/camoufox-mcp/package-lock.json /opt/camoufox-mcp/
RUN apt-get update && apt-get install -y --no-install-recommends xvfb xauth \
    && playwright install-deps firefox \
    && cd /opt/camoufox-mcp && npm ci --ignore-scripts --no-fund --no-audit \
    && npx camoufox-js fetch && npx camoufox-js version \
    && npm cache clean --force && rm -rf /var/lib/apt/lists/*
COPY tools/camoufox-mcp/wrap.mjs /opt/camoufox-mcp/wrap.mjs
# Claude Code CLI backs the claude-cli LLM provider; it reads the login mounted by docker-compose.
RUN npm install -g @anthropic-ai/claude-code@latest && claude --version && npm cache clean --force
WORKDIR /app
COPY --from=backend /scopeweaver /app/scopeweaver
# The supervisor forwards SIGTERM and restarts after a self-update.
COPY start.sh /app/start.sh
RUN chmod +x /app/scopeweaver /app/start.sh
COPY skills/ /app/skills/
COPY LICENSE /app/LICENSE
COPY THIRD_PARTY_NOTICES.txt /app/THIRD_PARTY_NOTICES.txt
VOLUME ["/app/data"]
EXPOSE 8787 8788
ENTRYPOINT ["/app/start.sh"]
CMD ["-addr", ":8787", "-proxy", ":8788"]
