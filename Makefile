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
#   make clean         - remove backend/web/dist and the deps-checked sentinel
#   make doctor        - run scripts/check-build-deps.sh to verify toolchain
#
# Build-time preflight: the first `make build` after `make clean` runs
# scripts/check-build-deps.sh to verify go / node / npm / git are present.
# Set INSTALL=1 to auto-install missing tools via apt/brew/winget:
#   INSTALL=1 make build
# Set SKIP_DEPS_CHECK=1 to bypass the check (CI cache, dev loop).
#
# If you change scripts/check-build-deps.sh, `make clean` first to re-verify.
.PHONY: build build-frontend build-backend run clean doctor

SENTINEL := .deps-checked
$(SENTINEL): scripts/check-build-deps.sh
ifndef SKIP_DEPS_CHECK
ifeq ($(INSTALL),1)
	@scripts/check-build-deps.sh --install --with-frontend
else
	@scripts/check-build-deps.sh --with-frontend
endif
endif
	@touch $(SENTINEL)

build: build-frontend build-backend
build-frontend build-backend: $(SENTINEL)

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
	rm -rf backend/web/dist dist/nova $(SENTINEL)

doctor:
	@scripts/check-build-deps.sh --with-frontend
