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
metadata_file="$root/release-metadata.env"

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

[[ -f $metadata_file ]] || {
  echo "required release metadata file is missing: $metadata_file" >&2
  exit 1
}
declare -A metadata=()
metadata_assignments=0
while IFS= read -r line || [[ -n $line ]]; do
  [[ $line =~ ^[[:space:]]*($|#) ]] && continue
  if [[ $line =~ ^(OLP_PREVIOUS_RELEASE_MODE|OLP_PREVIOUS_RELEASED_VERSION|OLP_PREVIOUS_RELEASED_IMAGE|OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION)=(.*)$ ]]; then
    key=${BASH_REMATCH[1]}
    value=${BASH_REMATCH[2]}
    [[ ! -v metadata["$key"] ]] || {
      echo "release metadata repeats $key" >&2
      exit 1
    }
    metadata["$key"]=$value
    ((metadata_assignments += 1))
    continue
  fi
  echo "release metadata contains an unsupported line: $line" >&2
  exit 1
done < "$metadata_file"

(( metadata_assignments == 4 )) || {
  echo "release metadata must contain exactly the four documented assignments" >&2
  exit 1
}
mode=${metadata[OLP_PREVIOUS_RELEASE_MODE]:-}
previous_version=${metadata[OLP_PREVIOUS_RELEASED_VERSION]:-}
previous_image=${metadata[OLP_PREVIOUS_RELEASED_IMAGE]:-}
previous_migration=${metadata[OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION]:-}
[[ $previous_migration =~ ^[0-9]{4}$ ]] || {
  echo "OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION must be four digits" >&2
  exit 1
}
case "$mode" in
  bootstrap)
    [[ $workspace_version == 2.0.0 ]] || {
      echo "bootstrap release metadata is permitted only for workspace version 2.0.0" >&2
      exit 1
    }
    [[ $previous_version == none && $previous_image == none && $previous_migration == 0021 ]] || {
      echo "the 2.0.0 bootstrap baseline must be version=none, image=none, migration=0021" >&2
      exit 1
    }
    ;;
  released)
    [[ $previous_version =~ $semver && $previous_version != "$workspace_version" ]] || {
      echo "released mode requires a semantic previous version different from $workspace_version" >&2
      exit 1
    }
    first_version=$(printf '%s\n%s\n' "$previous_version" "$workspace_version" | sort -V | head -n 1)
    [[ $first_version == "$previous_version" ]] || {
      echo "released mode previous version $previous_version is not older than $workspace_version" >&2
      exit 1
    }
    [[ $previous_image =~ ^ghcr\.io/tyk-swe/olp@sha256:[0-9a-f]{64}$ ]] || {
      echo "released mode requires an immutable ghcr.io/tyk-swe/olp image digest" >&2
      exit 1
    }
    ;;
  *)
    echo "OLP_PREVIOUS_RELEASE_MODE must be bootstrap or released" >&2
    exit 1
    ;;
esac

echo "release metadata is consistent at $workspace_version"
