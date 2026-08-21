# 3-stage Dockerfile, build context is the project root (.):
#   stage 1 (node:22-alpine)  builds the frontend SPA
#   stage 2 (golang:1.25)    embeds the SPA and builds the Go binary
#   stage 3 (node:22-alpine)  runs the single binary on :9527 with claude CLI
# pre-installed.  Replaces the original 2-stage build (which produced a
# backend-only image and required a separate nginx service to serve the
# frontend — see docker-compose.yml's now-removed frontend svc).
FROM node:22-alpine AS frontend
WORKDIR /app
COPY frontend/package*.json frontend/.npmrc ./
# `npm ci` aborts the build whenever package.json drifts from
# package-lock.json (common on feature branches).  `npm install` is more
# forgiving and `--no-audit --no-fund` keeps logs small.
ENV NODE_OPTIONS=--max-old-space-size=4096
RUN npm install --no-audit --no-fund
COPY frontend/. .
RUN npm run build

FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/. .
# backend/web/embed.go does `//go:embed all:dist`; the placeholder .gitkeep
# is overwritten by the COPY below so the embed resolves to the real SPA.
COPY --from=frontend /app/dist ./web/dist/
RUN CGO_ENABLED=0 go build -o /app/server ./cmd/server

FROM node:22-alpine
# Run as a non-root user. The Claude CLI refuses
# `--dangerously-skip-permissions` when uid==0 regardless of any env
# override, so the server MUST start as an unprivileged user inside the
# container. su-exec is used by the entrypoint to drop from the build-time
# root user into the runtime nova user after chowning the bind-mounted
# volumes (see entrypoint.sh).
#
# UID 1000 is fixed so host bind-mounts can be chowned to the same number
# via `chown -R 1000:1000 ~/.novaworkbench`. Override with NOVA_UID at
# build/run time if you need a different mapping on the host.
RUN apk add --no-cache git ca-certificates openssh-client su-exec tini
# tini forwards signals and reaps zombies — Claude spawns subagents that
# `claude -p` waits on; without tini the bash tool calls can leak zombies.
#
# Create the runtime user. We do the heavy `npm install -g` as root because
# `/usr/local/lib/node_modules` is owned by root, then chown the install to
# the nova user so a `USER nova` process can write to it later (e.g.
# `claude` self-update via the preflight auto-install path).
ENV NOVA_UID=1000
RUN addgroup -g ${NOVA_UID} -S nova \
    && adduser -u ${NOVA_UID} -G nova -D -h /home/nova -s /bin/sh nova
# Pre-install Claude CLI as root (writable /usr/local), then chown the
# install so nova can read+write it. ANTHROPIC_AUTH_TOKEN is injected at
# runtime via docker-compose env, never baked into the image.
RUN npm install -g @anthropic-ai/claude-code --no-audit --no-fund \
    && chown -R nova:nova /usr/local/lib/node_modules /usr/local/bin /usr/local/share
WORKDIR /app
# Copy the server binary from the builder stage. Build context is the repo
# root for both docker-compose (docker-compose.yml → context: .) and the CI
# workflow (.github/workflows/deploy.yml → context: .), so paths in COPY are
# anchored there: frontend/... in stage 1, backend/... in stage 2 and 3.
COPY --from=builder --chown=nova:nova /app/server ./
# docker-entrypoint.sh lives at backend/docker-entrypoint.sh in the repo;
# COPY it explicitly to /app/docker-entrypoint.sh so the ENTRYPOINT path
# below stays unchanged. (Using './docker-entrypoint.sh' as the destination
# preserves the `backend/` prefix and lands at /app/backend/... instead.)
COPY --chown=nova:nova backend/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh
EXPOSE 9527
# HEALTHCHECK uses wget — no shell dependency; the entrypoint finishes the
# chown/su-exec handoff before this can succeed.
HEALTHCHECK --interval=10s --timeout=5s --retries=3 \
    CMD wget -qO- http://localhost:9527/api/health || exit 1
ENTRYPOINT ["/sbin/tini", "--", "/app/docker-entrypoint.sh"]
CMD ["./server"]