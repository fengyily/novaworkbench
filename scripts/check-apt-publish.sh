#!/usr/bin/env bash
# scripts/check-apt-publish.sh
#
# End-to-end smoke check for the apt repo published by .github/workflows/release.yml.
# Run this locally after a release tag to confirm the gh-pages branch is reachable
# and serves a usable apt source. Exits 0 on success, non-zero on the first failure.

set -euo pipefail

: "${OWNER:=fengyily}"
: "${REPO:=novaworkbench}"
: "${PAGES_HOST:=https://${OWNER}.github.io/${REPO}}"
: "${BRANCH:=gh-pages}"

echo "==> checking ${BRANCH} branch HEAD on origin"
git ls-remote --heads "https://github.com/${OWNER}/${REPO}.git" "${BRANCH}" \
  | grep -q "refs/heads/${BRANCH}" \
  || { echo "::error::${BRANCH} branch missing on origin"; exit 2; }

for path in \
  "pool/main/n/nova/binary-amd64/Packages" \
  "pool/main/n/nova/binary-arm64/Packages" \
  "dists/stable/main/binary-amd64/Packages" \
  "dists/stable/main/binary-arm64/Packages" \
  "dists/stable/Release"; do
  url="${PAGES_HOST}/apt/${path}"
  echo "==> GET ${url}"
  code=$(curl -sS -o /tmp/apt-check.out -w '%{http_code}' "${url}" || true)
  if [[ "${code}" != "200" ]]; then
    echo "::error::${url} returned ${code}"
    exit 3
  fi
done

echo "==> apt source OK at ${PAGES_HOST}/apt/"
