#!/usr/bin/env bash
# scripts/gen-helloworld-test.sh
# 用法：bash scripts/gen-helloworld-test.sh
# 产物：./test/yyyyMMddHHmmss.html（内容为 "Hello workd"，本地时区时间戳）
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/test"
TS="$(date +%Y%m%d%H%M%S)"
OUT_FILE="${OUT_DIR}/${TS}.html"

mkdir -p "${OUT_DIR}"

# 注意：内容按用户原始描述保留 "Hello workd"（未修正拼写）
cat > "${OUT_FILE}" <<'HTML'
<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>Hello workd</title>
</head>
<body>
  Hello workd
</body>
</html>
HTML

echo "wrote ${OUT_FILE}"
