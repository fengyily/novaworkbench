#!/usr/bin/env bash
# Generate a one-shot HTML test page under ./test/ with content "Hello workd".
# Usage: ./scripts/gen-helloworld.sh  (or `make helloworld`)
#
# NOTE: "Hello workd" is the user's original typo — preserve it, do not fix.

set -euo pipefail

# Anchor the repo root via the script's own location, so callers can invoke
# from anywhere (including sub-directories) without breaking the path.
REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
TEST_DIR="$REPO_ROOT/test"

# Idempotent: no error if ./test/ already exists.
mkdir -p "$TEST_DIR"

# Local-time timestamp matching the yyyyMMDDhhmmss contract.
TS=$(date +%Y%m%d%H%M%S)
OUT="$TEST_DIR/${TS}.html"

# Quoted heredoc (<<'EOF') prevents shell expansion inside the HTML body,
# keeping the literal "Hello workd" exactly as written.
cat <<'EOF' > "$OUT"
<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Hello workd</title>
</head>
<body>
<p>Hello workd</p>
</body>
</html>
EOF

echo "Generated: $OUT"