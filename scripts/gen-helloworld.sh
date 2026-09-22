#!/usr/bin/env bash
# Generate a HelloWorld test page under ./test/ with timestamp filename.
# Filename format: yyyyMMddHHmmss.html
# Content matches the existing test/*.html template (Hello workd typo preserved).

set -euo pipefail

# 1. 定位仓库根目录（脚本无论从哪执行都能找到 ./test）
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_dir="${repo_root}/test"

# 2. 确保 test 目录存在
mkdir -p "${test_dir}"

# 3. 生成时间戳（macOS/Linux 兼容写法）
timestamp="$(date +"%Y%m%d%H%M%S")"

out_file="${test_dir}/${timestamp}.html"

# 4. 写入标准 HTML 模板（与现有 test/*.html 内容完全一致，无末尾换行）
printf '%s' '<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<title>Hello world test page</title>
</head>
<body>
Hello workd
</body>
</html>' > "${out_file}"

# 5. 输出结果
echo "✓ Generated: ${out_file}"