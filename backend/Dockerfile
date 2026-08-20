# 3-stage Dockerfile: node builds the frontend SPA, Go embeds it, alpine
# runs the single binary on :9527. Replaces the original 2-stage build
# (which produced a backend-only image and required a separate nginx service
# to serve the frontend — see docker-compose.yml's now-removed frontend svc).
FROM node:22-alpine AS frontend
WORKDIR /app
COPY package*.json ./
RUN npm ci
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

FROM alpine:3.21
WORKDIR /app
COPY --from=builder /app/server .
# git/docker aren't part of the image — users install Claude CLI + Node on
# the host (or rely on the preflight auto-install path).
EXPOSE 9527
CMD ["./server"]
