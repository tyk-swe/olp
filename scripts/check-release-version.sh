#!/usr/bin/env bash
set -euo pipefail

if [[ $# -gt 1 || ${1:-} == --help || ${1:-} == -h ]]; then
  echo "usage: $0 [EXPECTED_VERSION]" >&2
  [[ $# -eq 1 ]] && exit 0 || exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
expected=${1:-}

workspace_version=$(awk '
  /^\[workspace.package\]$/ { workspace = 1; next }
  /^\[/ { workspace = 0 }
  workspace && /^version = "/ {
    value = $0
    sub(/^version = "/, "", value)
    sub(/"$/, "", value)
    print value
    exit
  }
' "$root/Cargo.toml")
console_version=$(sed -nE 's/^  "version": "([^"]+)",$/\1/p' "$root/console/package.json")
chart_version=$(sed -nE 's/^version: "?([^"[:space:]]+)"?$/\1/p' "$root/deploy/helm/Chart.yaml")
chart_app_version=$(sed -nE 's/^appVersion: "?([^"[:space:]]+)"?$/\1/p' "$root/deploy/helm/Chart.yaml")
image_version=$(sed -nE 's/^ARG OLP_VERSION=([^[:space:]]+)$/\1/p' "$root/deploy/Dockerfile")

semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
[[ $workspace_version =~ $semver ]] || {
  echo "workspace version is not semantic: $workspace_version" >&2
  exit 1
}

for pair in \
  "console/package.json:$console_version" \
  "deploy/helm/Chart.yaml version:$chart_version" \
  "deploy/helm/Chart.yaml appVersion:$chart_app_version" \
  "deploy/Dockerfile OLP_VERSION:$image_version"; do
  label=${pair%%:*}
  value=${pair#*:}
  [[ $value == "$workspace_version" ]] || {
    echo "$label is $value, expected $workspace_version" >&2
    exit 1
  }
done

if [[ -n $expected && $workspace_version != "$expected" ]]; then
  echo "release tag version $expected does not match package version $workspace_version" >&2
  exit 1
fi

if rg -n 'path = "[^"]+", version = "(?!'"$workspace_version"')' \
  "$root/Cargo.toml" --pcre2; then
  echo "a workspace path dependency does not match $workspace_version" >&2
  exit 1
fi

echo "release metadata is consistent at $workspace_version"
