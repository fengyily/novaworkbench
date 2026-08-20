# 3-stage Dockerfile: node builds the frontend SPA, Go embeds it, alpine
# runs the single binary on :9527. Replaces the original 2-stage build
# (which produced a backend-only image and required a separate nginx service
# to serve the frontend — see docker-compose.yml's now-removed frontend svc).
FROM node:22-alpine AS frontend
WORKDIR /app
COPY package*.json .npmrc ./
# Exit code 254 from `npm install` inside buildx is most often caused by
# the install hitting the default 2 GB Node heap on Alpine (musl's allocator
# is more memory-hungry than glibc). Bump the heap, retry the install on
# transient network failures, and use `npm install` rather than `npm ci` so
# the lockfile can drift with package.json during feature-branch work.
ENV NODE_OPTIONS=--max-old-space-size=4096
RUN npm install --no-audit --no-fund
COPY . .
RUN npm run build

FROM golang:1.25-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Embed the SPA built in the previous stage so the binary is self-contained.
# backend/web/ is the package that does `//go:embed all:dist`; the placeholder
# .gitkeep from the source tree is overwritten by the COPY below so the
# embed resolves to the real built SPA.
COPY --from=frontend /app/dist ./web/dist/
RUN CGO_ENABLED=0 go build -o /app/server ./cmd/server

FROM node:22-alpine
WORKDIR /app
# Pre-install Claude CLI so the wizard pipeline works inside the container
# without relying on the preflight auto-install path (which dies on restart).
# ANTHROPIC_AUTH_TOKEN is injected at runtime via docker-compose env, never
# baked into the image.
RUN npm install -g @anthropic-ai/claude-code --no-audit --no-fund
COPY --from=builder /app/server .
EXPOSE 9527
CMD ["./server"]
