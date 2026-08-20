#!/usr/bin/env bash
# Build-time dependency check. Ensures `make build` / `build-all.sh` have the
# toolchain they need (go, node, npm, git). Runs as a prerequisite at the
# top of every build entry point so users get a clear, actionable error
# instead of a cryptic `command not found` two layers deep.
#
# Usage:
#   scripts/check-build-deps.sh                    # status report, exit 1 if missing
#   scripts/check-build-deps.sh --install          # also auto-install missing tools
#   scripts/check-build-deps.sh --with-frontend    # stricter check (needs node + npm)
#
# Minimum versions reflect the project's toolchain:
#   - Go 1.22+ (project uses Go 1.22 routing features; go.mod says 1.25)
#   - Node 20+ (Vite 8 requires Node 20.19+ or 22.12+)
#   - npm (bundled with Node)
#   - git (used by the runner handler to inspect project worktrees)
set -euo pipefail

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
WITH_FRONTEND=false
INSTALL=false
for arg in "$@"; do
  case "$arg" in
    --with-frontend) WITH_FRONTEND=true ;;
    --install)       INSTALL=true ;;
    -h|--help)
      sed -n '2,18p' "$0"
      exit 0
      ;;
  esac
done

# ANSI helpers (no-op when stdout isn't a tty)
if [[ -t 1 ]]; then
  R='\033[0m'; G='\033[32m'; Y='\033[33m'; R='\033[31m'; B='\033[1m'
else
  R=''; G=''; Y=''; R=''; B=''
fi
ok()   { printf "${G}✓${R} %-12s %s\n" "$1" "$2"; }
warn() { printf "${Y}⚠${R} %-12s %s\n" "$1" "$2"; }
fail() { printf "${R}✗${R} %-12s %s\n" "$1" "$2"; }

# ---- version helpers ------------------------------------------------------

min_go=1.22
min_node=20

cmp_ver() {
  # returns 0 (ok) / 1 (too old). arg: actual, required
  local actual="$1" required="$2"
  local highest=$(printf '%s\n%s\n' "$actual" "$required" | sort -V | tail -1)
  [[ "$highest" == "$actual" ]]
}

get_go_version() {
  if command -v go >/dev/null 2>&1; then
    go version | awk '{print $3}' | sed 's/^go//'
  else
    echo ""
  fi
}

get_node_version() {
  if command -v node >/dev/null 2>&1; then
    node --version | sed 's/^v//'
  else
    echo ""
  fi
}

get_npm_version() {
  if command -v npm >/dev/null 2>&1; then
    npm --version
  else
    echo ""
  fi
}

# ---- platform-specific install --------------------------------------------

install_with_apt() {
  warn "正在使用 apt-get 安装缺失工具（需要 sudo）…"
  sudo apt-get update -y
  sudo apt-get install -y "$@"
}
install_with_dnf() { warn "正在使用 dnf 安装…（需要 sudo）"; sudo dnf install -y "$@"; }
install_with_yum() { warn "正在使用 yum 安装…（需要 sudo）"; sudo yum install -y "$@"; }
install_with_brew() { warn "正在使用 brew 安装…"; brew install "$@"; }
install_with_winget() { warn "正在使用 winget 安装…"; winget install --id "$@" -e --accept-source-agreements --accept-package-agreements; }

detect_pkg_manager() {
  case "$OS" in
    linux)
      for pm in apt-get dnf yum pacman; do
        if command -v "$pm" >/dev/null 2>&1; then
          echo "$pm"; return
        fi
      done
      ;;
    darwin)
      if command -v brew >/dev/null 2>&1; then echo "brew"; return; fi
      ;;
    mingw*|msys*|cygwin*) echo "winget" ;;
  esac
  echo ""
}

install_pkgs() {
  local pm; pm=$(detect_pkg_manager)
  if [[ -z "$pm" ]]; then
    fail "auto-install" "未识别的包管理器，请手动安装: $*"
    return 1
  fi
  case "$pm" in
    apt-get) install_with_apt "$@" ;;
    dnf)     install_with_dnf "$@" ;;
    yum)     install_with_yum "$@" ;;
    brew)    install_with_brew "$@" ;;
    winget)
      # winget takes --id; map common packages to IDs
      local id_args=()
      for p in "$@"; do
        case "$p" in
          node)     id_args+=("OpenJS.NodeJS.LTS") ;;
          npm)      id_args+=("OpenJS.NodeJS.LTS") ;;  # npm ships with node
          git)      id_args+=("Git.Git") ;;
          golang-go) id_args+=("GoLang.Go") ;;
          *)        id_args+=("$p") ;;
        esac
      done
      install_with_winget "${id_args[@]}"
      ;;
  esac
}

# Map go / node package names per platform
pkg_for() {
  local tool="$1"
  case "$OS:$tool" in
    linux:go)        command -v apt-get >/dev/null 2>&1 && echo "golang-go" || echo "go" ;;
    linux:node)      echo "nodejs" ;;   # apt; npm comes via separate npm pkg on Debian
    linux:npm)       echo "npm" ;;
    linux:git)       echo "git" ;;
    darwin:*)        echo "$tool" ;;    # brew uses formula names (go, node, git)
    mingw*|msys*|cygwin*:*) echo "$tool" ;;
    *)               echo "$tool" ;;
  esac
}

# ---- checks ---------------------------------------------------------------

missing=()
GO_VER=$(get_go_version)
NODE_VER=$(get_node_version)
NPM_VER=$(get_npm_version)

# go
if [[ -z "$GO_VER" ]]; then
  fail "go"   "未安装"; missing+=(go)
else
  if cmp_ver "$GO_VER" "$min_go.0"; then
    ok "go"     "v${GO_VER} (≥ ${min_go})"
  else
    fail "go"   "v${GO_VER} 低于要求的 ${min_go}"; missing+=(go)
  fi
fi

# node (only if frontend requested)
if $WITH_FRONTEND; then
  if [[ -z "$NODE_VER" ]]; then
    fail "node" "未安装"; missing+=(node)
  else
    if cmp_ver "$NODE_VER" "$min_node.0"; then
      ok "node"  "v${NODE_VER} (≥ ${min_node})"
    else
      fail "node" "v${NODE_VER} 低于要求的 ${min_node}"; missing+=(node)
    fi
  fi
  if [[ -z "$NPM_VER" ]]; then
    fail "npm"  "未安装"; missing+=(npm)
  else
    ok "npm"    "v${NPM_VER}"
  fi
else
  if [[ -n "$NODE_VER" ]]; then
    ok "node"   "v${NODE_VER}（前端构建需要；用 --with-frontend 启用检查）"
  else
    warn "node" "未安装（仅在 --with-frontend 时需要）"
  fi
fi

# git
if command -v git >/dev/null 2>&1; then
  ok "git"   "$(git --version)"
else
  warn "git"  "未安装（运行时也需要；wizard 的 git 检查可能失败）"; missing+=(git)
fi

# ---- auto-install ---------------------------------------------------------

if [[ ${#missing[@]} -gt 0 ]]; then
  echo ""
  if $INSTALL; then
    echo ">> 自动安装: ${missing[*]}"
    pkgs=()
    for t in "${missing[@]}"; do pkgs+=("$(pkg_for "$t")"); done
    install_pkgs "${pkgs[@]}" || true
    # Re-check
    GO_VER=$(get_go_version); NODE_VER=$(get_node_version); NPM_VER=$(get_npm_version)
    echo ""
    echo ">> 重新检查："
    if [[ -z "$GO_VER" ]]; then fail "go" "仍未安装"; exit 1; fi
    if $WITH_FRONTEND; then
      if [[ -z "$NODE_VER" || -z "$NPM_VER" ]]; then fail "node/npm" "仍未安装"; exit 1; fi
    fi
  else
    echo ""
    fail "build-deps" "缺少: ${missing[*]}"
    echo ""
    case "$OS" in
      linux)
        if command -v apt-get >/dev/null 2>&1; then
          echo "  ${B}自动安装：${R}   scripts/check-build-deps.sh --install --with-frontend"
          echo "  ${B}手动安装：${R}   sudo apt-get install -y golang-go nodejs npm git"
        elif command -v dnf >/dev/null 2>&1; then
          echo "  ${B}手动安装：${R}   sudo dnf install -y golang nodejs npm git"
        elif command -v yum >/dev/null 2>&1; then
          echo "  ${B}手动安装：${R}   sudo yum install -y golang nodejs npm git"
        fi
        ;;
      darwin)
        echo "  ${B}自动安装：${R}   scripts/check-build-deps.sh --install --with-frontend"
        echo "  ${B}手动安装：${R}   brew install go node git"
        ;;
      mingw*|msys*|cygwin*)
        echo "  ${B}自动安装：${R}   scripts/check-build-deps.sh --install --with-frontend"
        echo "  ${B}手动安装：${R}   winget install GoLang.Go OpenJS.NodeJS.LTS Git.Git"
        ;;
    esac
    exit 1
  fi
fi

echo ""
ok "全部就绪" "可以开始构建"
