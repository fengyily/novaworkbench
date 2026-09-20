#!/usr/bin/env bash
# gen-hello-test-page.sh — Generate a timestamped HelloWorld test page under ./test/.
#
# Usage: bash scripts/gen-hello-test-page.sh

set -euo pipefail

OUTPUT_DIR="./test"
TIMESTAMP="$(date +%Y%m%d%H%M%S)"
OUTPUT_FILE="${OUTPUT_DIR}/${TIMESTAMP}.html"

mkdir -p "${OUTPUT_DIR}"

cat > "${OUTPUT_FILE}" <<'HTML'
<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Hello workd</title>
</head>
<body>
  <p>Hello workd</p>
</body>
</html>
HTML

echo "wrote ${OUTPUT_FILE}"
