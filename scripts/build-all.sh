#!/usr/bin/env bash
# Cross-compile NovaWorkbench for Linux/Windows/macOS on x86_64 and arm64.
# Produces single-binary releases into dist/ (each binary ships the embedded
# frontend SPA, so the user just downloads one file per platform).
#
# Prerequisites: run `make build` first (to populate backend/web/dist), or
# run `scripts/build-all.sh --with-frontend` to do the frontend build inline.
set -euo pipefail
cd "$(dirname "$0")/.."

if [[ "${1:-}" == "--with-frontend" || ! -f backend/web/dist/index.html ]]; then
  echo ">> building frontend..."
  (cd frontend && npm ci && npm run build)
  rm -rf backend/web/dist
  mkdir -p backend/web/dist
  cp -r frontend/dist/. backend/web/dist/
fi

mkdir -p dist

# Pair list: go GOOS/GOARCH + binary extension. arm64 entries are first so
# Apple Silicon users get the right download by default.
TARGETS=(
  "darwin/arm64/"
  "darwin/amd64/"
  "linux/amd64/"
  "linux/arm64/"
  "windows/amd64/.exe"
)

for t in "${TARGETS[@]}"; do
  GOOS="${t%%/*}"
  ARCH_AND_EXT="${t#*/}"
  GOARCH="${ARCH_AND_EXT%%/*}"
  EXT="${ARCH_AND_EXT#*/}"

  OUT="dist/novaworkbench-${GOOS}-${GOARCH}${EXT}"
  echo ">> building ${OUT}"
  (cd backend && CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -o "../${OUT}" ./cmd/server)
done

echo ""
echo "Done. Outputs in dist/:"
ls -lh dist/novaworkbench-*
