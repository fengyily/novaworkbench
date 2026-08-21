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
WORKDIR /app
# Alpine ships without git, but the project add flow shells out to
# `git init` / `git clone` (project.go, fs.go, review.go, wizard.go, …).
# Without git on PATH, "添加项目" returns ADD_FAILED: exec: "git":
# executable file not found in $PATH. ca-certificates is required for
# https:// clone remotes to verify TLS.
RUN apk add --no-cache git ca-certificates
# Pre-install Claude CLI so the wizard pipeline works inside the container
# without relying on the preflight auto-install path (which dies on restart).
# ANTHROPIC_AUTH_TOKEN is injected at runtime via docker-compose env, never
# baked into the image.
RUN npm install -g @anthropic-ai/claude-code --no-audit --no-fund
COPY --from=builder /app/server .
EXPOSE 9527
CMD ["./server"]