#!/usr/bin/env bash
# Cross-compile NovaWorkbench for Linux/Windows/macOS on x86_64 and arm64.
# Produces single-binary releases into dist/ (each binary ships the embedded
# frontend SPA, so the user just downloads one file per platform).
#
# Prerequisites: run `make build` first (to populate backend/web/dist), or
# run `scripts/build-all.sh --with-frontend` to do the frontend build inline.
#
# Flags:
#   --with-frontend   Build the frontend SPA first (needs node + npm) and
#                     embed it into every cross-compiled binary.
#   --install         Auto-install any missing build deps via the host's
#                     package manager (apt/brew/winget). Combines with
#                     --with-frontend.
#   --skip-deps-check Bypass the toolchain preflight (CI cache etc.).
set -euo pipefail
cd "$(dirname "$0")/.."

WITH_FRONTEND=false
INSTALL=false
SKIP_CHECK=false
for arg in "$@"; do
  case "$arg" in
    --with-frontend)   WITH_FRONTEND=true ;;
    --install)         INSTALL=true ;;
    --skip-deps-check) SKIP_CHECK=true ;;
    *) echo "unknown flag: $arg" >&2; exit 1 ;;
  esac
done

# Toolchain preflight. Runs once before any work; --install auto-installs
# missing tools via apt/brew/winget.
if ! $SKIP_CHECK; then
  ARGS=()
  $WITH_FRONTEND && ARGS+=(--with-frontend)
  $INSTALL       && ARGS+=(--install)
  scripts/check-build-deps.sh "${ARGS[@]}"
fi

if $WITH_FRONTEND || [[ ! -f backend/web/dist/index.html ]]; then
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
