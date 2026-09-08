#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${GITHUB_REPOSITORY:?set the repository containing the published API contract}"
baseline=$(gh release list --repo "$GITHUB_REPOSITORY" --exclude-drafts --exclude-pre-releases \
  --limit 100 --json tagName --jq '[.[] | select(.tagName | test("^v3\\.[0-9]+\\.[0-9]+$"))][0].tagName // empty')
if [[ -z $baseline ]]; then
  echo 'No published 3.x release exists; this release establishes the management API baseline.'
  exit 0
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
gh release download "$baseline" --repo "$GITHUB_REPOSITORY" --pattern management.json --dir "$work"
cp openapi/management.json "$work/candidate.json"
chmod 755 "$work"
chmod 644 "$work"/*.json
docker run --rm --mount "type=bind,src=$work,dst=/spec,readonly" \
  tufin/oasdiff@sha256:d67b83c56670e510e274d581a5c3c197b58fbbaffd72056bc61f8287ac1a7414 \
  breaking /spec/management.json /spec/candidate.json --fail-on ERR
