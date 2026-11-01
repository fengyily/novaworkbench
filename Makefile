SHELL := /bin/bash

# Build identity — injected via -ldflags into backend/internal/version so
# /api/health reflects the actual build. Override on the command line:
#   VERSION=v0.2.0 make build
VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w \
	-X github.com/novaworkbench/backend/internal/version.Version=$(VERSION) \
	-X github.com/novaworkbench/backend/internal/version.Commit=$(COMMIT) \
	-X github.com/novaworkbench/backend/internal/version.BuildDate=$(BUILD_DATE)

# Embed the frontend SPA into the Go binary (single-binary deployment).
# The frontend builds to frontend/dist/ (Vite default); we copy that into
# backend/web/dist/ where `//go:embed all:dist` picks it up at compile time.
#
# Targets:
#   make build         - frontend prod build + backend CGO_ENABLED=0 build
#   make build-backend - backend only (assumes backend/web/dist already exists;
#                       use this during pure backend dev with NOVA_SKIP_FRONTEND=1)
#   make run           - dev backend (no embed; vite handles the SPA on :5173).
#                       Override the listen port with PORT=<n> (default 9527):
#                         make run                # default 9527
#                         PORT=9000 make run      # listen on :9000
#                       The value is forwarded as NOVA_PORT, which the backend
#                       honors over the default 9527.
#   make clean         - remove backend/web/dist and the deps-checked sentinel
#   make doctor        - run scripts/check-build-deps.sh to verify toolchain
#
#   make ui-audit-deps - install the audit tool's browser driver (playwright-core)
#   make ui-audit      - run the global UI consistency audit against a running
#                       instance and open one issue per inconsistency.
#                       Override the target with BASE_URL / USERNAME / PASSWORD:
#                         make ui-audit BASE_URL=http://localhost:9527
#   make ui-audit-review  - re-run the model pass on an existing capture
#                       (RUN_ID=...); no browser needed.
#   make ui-audit-publish - push the evidence branch and open the issues for an
#                       existing capture (RUN_ID=...); no browser needed.
#   make ui-audit-check- CI gate: fail when a NEW UI convention violation appears
#                       (no browser, no model and no npm install needed).
#
# The frontend build runs through scripts/with-node.sh, which puts a
# Vite-compatible Node (20.19+ / 22.12+) on PATH — a machine whose default
# `node` is 18.x (Debian/Ubuntu apt) still builds as long as a newer Node is
# installed somewhere (nvm / fnm / volta / asdf / homebrew), or NOVA_NODE_BIN
# points at one.
#
# Build-time preflight: the first `make build` after `make clean` runs
# scripts/check-build-deps.sh to verify go / node / npm / git are present.
# Set INSTALL=1 to auto-install missing tools via apt/brew/winget:
#   INSTALL=1 make build
# Set SKIP_DEPS_CHECK=1 to bypass the check (CI cache, dev loop).
#
# If you change scripts/check-build-deps.sh, `make clean` first to re-verify.
.PHONY: build build-frontend build-backend run clean doctor ui-audit ui-audit-deps ui-audit-review ui-audit-publish ui-audit-check

SENTINEL := .deps-checked
$(SENTINEL): scripts/check-build-deps.sh scripts/node-env.sh
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
	cd frontend && ../scripts/with-node.sh npm ci && ../scripts/with-node.sh npm run build
	rm -rf backend/web/dist
	mkdir -p backend/web/dist
	cp -r frontend/dist/. backend/web/dist/

build-backend:
	cd backend && CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o ../dist/nova ./cmd/server
	@echo "Built: dist/nova (version=$(VERSION) commit=$(COMMIT))"

run:
	cd backend && NOVA_PORT=$${PORT:-9527} go run ./cmd/server

clean:
	rm -rf backend/web/dist dist/nova $(SENTINEL)

doctor:
	@scripts/check-build-deps.sh --with-frontend

# Global UI consistency audit (scripts/ui-audit). The tool needs a running
# instance of the app; it drives a headless browser over every route,
# compares the computed styles of shared controls page by page, and files
# one GitHub issue per confirmed inconsistency with the screenshot attached.
ui-audit-deps:
	npm --prefix scripts/ui-audit ci

ui-audit: ui-audit-deps
	node scripts/ui-audit/run.mjs all --base-url $(BASE_URL) $(if $(USERNAME),--username $(USERNAME),) $(if $(PASSWORD),--password $(PASSWORD),) $(if $(SEED),--seed,) $(if $(RUN_ID),--run-id $(RUN_ID),)

# Review and publish an already-captured run. Reusing the evidence avoids
# driving the browser again when only the wording or the issue set changed.
ui-audit-review:
	@test -n "$(RUN_ID)" || (echo "RUN_ID is required (e.g. make ui-audit-review RUN_ID=20261025-2015)" && exit 1)
	node scripts/ui-audit/run.mjs review --run-id $(RUN_ID)

ui-audit-publish:
	@test -n "$(RUN_ID)" || (echo "RUN_ID is required (e.g. make ui-audit-publish RUN_ID=20261025-2015)" && exit 1)
	node scripts/ui-audit/run.mjs publish --run-id $(RUN_ID) $(if $(MIN_SEVERITY),--min-severity $(MIN_SEVERITY),) $(if $(MAX_ISSUES),--max-issues $(MAX_ISSUES),)

ui-audit-check:
	node scripts/ui-audit/run.mjs check
