#!/usr/bin/env bash
# Shared Node.js resolution helpers. Source this file; it defines functions
# only and never exits on its own.
#
# Why this exists: the frontend toolchain (Vite 8) requires Node 20.19+ or
# 22.12+. Distro packages (Debian/Ubuntu apt) still ship Node 18, and a
# non-interactive build (make, CI runner, nova coding job) does not source
# the user's shell rc, so an nvm/fnm/volta-managed Node never reaches PATH.
# The result is a cryptic crash deep inside vite:
#
#   You are using Node.js 18.20.8. Vite requires Node.js version 20.19+ ...
#   ReferenceError: CustomEvent is not defined
#
# nova_find_node_bin scans the usual version-manager layouts and prints the
# bin directory of the newest Node that satisfies the requirement, so build
# entry points can prepend it to PATH (see scripts/with-node.sh).
#
# Functions:
#   nova_node_version_ok <version>  -> 0 when the version satisfies Vite
#   nova_node_requirement           -> human-readable requirement string
#   nova_find_node_bin              -> prints a usable node bin dir, or ""
#   nova_activate_node              -> prepends that dir to PATH (0/1)

NOVA_NODE_MIN_20_MINOR=19   # 20.19+
NOVA_NODE_MIN_22_MINOR=12   # 22.12+

nova_node_requirement() {
  echo "Node 20.19+ 或 22.12+ (Vite 8 要求)"
}

# nova_node_version_ok <x.y.z|vx.y.z>
nova_node_version_ok() {
  local v="${1#v}"
  local major="${v%%.*}"
  local rest="${v#*.}"
  local minor="${rest%%.*}"
  [[ "$major" =~ ^[0-9]+$ ]] || return 1
  [[ "$minor" =~ ^[0-9]+$ ]] || minor=0
  if (( major > 22 )); then return 0; fi
  if (( major == 22 )); then (( minor >= NOVA_NODE_MIN_22_MINOR )); return; fi
  if (( major == 20 )); then (( minor >= NOVA_NODE_MIN_20_MINOR )); return; fi
  # 21.x / 23.x are odd-numbered lines Vite does not target, everything
  # below 20.19 is plainly too old.
  return 1
}

# Prints the version of the given node binary ("" when it can't run).
nova_node_binary_version() {
  local bin="$1"
  [[ -x "$bin" ]] || return 0
  "$bin" --version 2>/dev/null | sed 's/^v//'
}

# Prints candidate node binaries, newest version manager entries first.
_nova_node_candidates() {
  local dir

  # Explicit override always wins: NOVA_NODE_BIN may be the binary itself
  # or the directory containing it.
  if [[ -n "${NOVA_NODE_BIN:-}" ]]; then
    if [[ -d "$NOVA_NODE_BIN" ]]; then
      echo "$NOVA_NODE_BIN/node"
    else
      echo "$NOVA_NODE_BIN"
    fi
  fi

  # Whatever the current PATH resolves to — preferred when it's new enough
  # so we don't silently switch the user's toolchain.
  command -v node 2>/dev/null

  # nvm / fnm / volta / asdf layouts. `sort -rV` puts the newest first.
  for dir in "${NVM_DIR:-$HOME/.nvm}/versions/node"; do
    [[ -d "$dir" ]] || continue
    find "$dir" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | sort -rV | sed 's|$|/bin/node|'
  done
  for dir in "${FNM_DIR:-$HOME/.fnm}/node-versions" "$HOME/.local/share/fnm/node-versions"; do
    [[ -d "$dir" ]] || continue
    find "$dir" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | sort -rV | sed 's|$|/installation/bin/node|'
  done
  for dir in "${VOLTA_HOME:-$HOME/.volta}/tools/image/node"; do
    [[ -d "$dir" ]] || continue
    find "$dir" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | sort -rV | sed 's|$|/bin/node|'
  done
  for dir in "$HOME/.asdf/installs/nodejs"; do
    [[ -d "$dir" ]] || continue
    find "$dir" -maxdepth 1 -mindepth 1 -type d 2>/dev/null | sort -rV | sed 's|$|/bin/node|'
  done

  # Well-known system locations (homebrew keg-only formulae included).
  echo "/opt/homebrew/opt/node@24/bin/node"
  echo "/opt/homebrew/opt/node@22/bin/node"
  echo "/opt/homebrew/opt/node@20/bin/node"
  echo "/usr/local/opt/node@24/bin/node"
  echo "/usr/local/opt/node@22/bin/node"
  echo "/usr/local/opt/node@20/bin/node"
  echo "/opt/homebrew/bin/node"
  echo "/usr/local/bin/node"
  echo "/usr/bin/node"
  echo "/snap/bin/node"
}

# Prints the bin directory of the first candidate that satisfies the
# requirement. Prints nothing (and returns 1) when none does.
nova_find_node_bin() {
  local bin ver
  while read -r bin; do
    [[ -n "$bin" && -x "$bin" ]] || continue
    ver="$(nova_node_binary_version "$bin")"
    [[ -n "$ver" ]] || continue
    if nova_node_version_ok "$ver"; then
      dirname "$bin"
      return 0
    fi
  done < <(_nova_node_candidates)
  return 1
}

# Prepends a usable node bin dir to PATH. Returns 1 when none was found.
nova_activate_node() {
  local bindir
  bindir="$(nova_find_node_bin)" || return 1
  case ":$PATH:" in
    *":$bindir:"*) ;;
    *) PATH="$bindir:$PATH"; export PATH ;;
  esac
  return 0
}

# Multi-line hint printed when no usable Node exists on the machine.
nova_node_install_hint() {
  cat <<'EOF'
  自动安装：   scripts/check-build-deps.sh --install --with-frontend
  或使用 nvm： curl -fsSL https://raw.githubusercontent.com/nvm-sh/nvm/v0.40.1/install.sh | bash
               export NVM_DIR="$HOME/.nvm"; . "$NVM_DIR/nvm.sh"; nvm install --lts
  已装好但未进 PATH 时，可显式指定：NOVA_NODE_BIN=/path/to/node/bin make build
EOF
}
