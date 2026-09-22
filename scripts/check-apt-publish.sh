#!/usr/bin/env bash
# scripts/check-apt-publish.sh
#
# End-to-end smoke check for the standalone apt repo published by
# .github/workflows/release.yml to github.com/fengyily/linux-repo.
# This script ONLY validates fengyily/linux-repo — the previous
# OWNER/REPO overrides have been removed because the release pipeline
# publishes exclusively to fengyily/linux-repo (see docs/RELEASE.md).
# Run locally after a release tag to confirm linux-repo is reachable and
# serves a usable apt source. Exits 0 on success, non-zero on the first failure.

set -euo pipefail

# Hard-coded: the only legal apt-publishing target. Do not reintroduce
# OWNER/REPO overrides — a typo here would silently mis-target the
# verification, and the release pipeline has no other target to validate.
readonly OWNER="fengyily"
readonly REPO="linux-repo"
readonly BRANCH="main"
readonly PAGES_HOST="https://${OWNER}.github.io/${REPO}"

# Refuse to run if the caller exports OWNER/REPO — they no longer take effect.
if env | grep -qE '^(OWNER|REPO|PAGES_HOST|BRANCH)='; then
  echo "::error::check-apt-publish.sh hard-codes OWNER/REPO; remove the env var(s) and rerun"
  exit 64
fi

echo "==> checking ${BRANCH} branch HEAD on ${OWNER}/${REPO}"
git ls-remote --heads "https://github.com/${OWNER}/${REPO}.git" "${BRANCH}" \
  | grep -q "refs/heads/${BRANCH}" \
  || { echo "::error::${BRANCH} branch missing on https://github.com/${OWNER}/${REPO}"; exit 2; }

# Sanity check: linux-repo must have GitHub Pages enabled (Branch: main / root).
# A 200 from the Pages host is the cheapest end-to-end proof; the per-file
# GETs below would still pass if Pages serves from a non-/apt subpath, so we
# do the Pages host probe first and bail with a specific error if it 404s.
echo "==> checking GitHub Pages is enabled on ${OWNER}/${REPO}"
code=$(curl -sS -o /dev/null -w '%{http_code}' "${PAGES_HOST}/" || true)
if [[ "${code}" != "200" ]]; then
  echo "::error::${PAGES_HOST}/ returned ${code} — enable Settings → Pages → Branch: main / (root) on ${OWNER}/${REPO}"
  exit 4
fi

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
