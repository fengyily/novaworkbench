SHELL := /bin/bash

# Embed the frontend SPA into the Go binary (single-binary deployment).
# The frontend builds to frontend/dist/ (Vite default); we copy that into
# backend/web/dist/ where `//go:embed all:dist` picks it up at compile time.
#
# Targets:
#   make build         - frontend prod build + backend CGO_ENABLED=0 build
#   make build-backend - backend only (assumes backend/web/dist already exists;
#                       use this during pure backend dev with NOVA_SKIP_FRONTEND=1)
#   make run           - dev backend (no embed; vite handles the SPA on :5173)
#   make clean         - remove backend/web/dist
.PHONY: build build-backend run clean

build: build-frontend build-backend

build-frontend:
	cd frontend && npm ci && npm run build
	rm -rf backend/web/dist
	mkdir -p backend/web/dist
	cp -r frontend/dist/. backend/web/dist/

build-backend:
	cd backend && CGO_ENABLED=0 go build -o ../dist/nova ./cmd/server
	@echo "Built: dist/nova"

run:
	cd backend && go run ./cmd/server

clean:
	rm -rf backend/web/dist dist/nova
