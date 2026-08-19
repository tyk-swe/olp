#!/usr/bin/env bash
set -euo pipefail

if [[ $# -gt 1 || ${1:-} == --help || ${1:-} == -h ]]; then
  echo "usage: $0 [EXPECTED_VERSION]" >&2
  [[ $# -eq 1 ]] && exit 0 || exit 2
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$script_dir/.." && pwd)
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"

for required_executable in rg awk sed dirname jq; do
  validation_require_executable "$required_executable"
done
for required_directory in "$root/console" "$root/deploy" "$root/deploy/helm"; do
  validation_require_directory "$required_directory"
done
for required_file in \
  "$root/Cargo.toml" "$root/console/package.json" \
  "$root/deploy/helm/Chart.yaml" "$root/deploy/Dockerfile" \
  "$root/release-metadata.env"; do
  validation_require_file "$required_file"
done

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
# jq: JSON parsing must not depend on indentation. The workspace TOML above
# stays awk-parsed because this script also runs in CI's quality job, which
# has jq but no cargo for `cargo metadata`.
console_version=$(jq -r '.version // empty' "$root/console/package.json")
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

version_mismatches=
version_mismatches_matched=
checked_rg_capture version_mismatches version_mismatches_matched \
  "scan workspace path dependency versions" "$root/Cargo.toml" \
  -n 'path = "[^"]+", version = "(?!'"$workspace_version"')' \
  "$root/Cargo.toml" --pcre2
if (( version_mismatches_matched )); then
  printf '%s\n' "$version_mismatches"
  echo "a workspace path dependency does not match $workspace_version" >&2
  exit 1
fi

metadata_file="$root/release-metadata.env"
prev_version=''
prev_migration=''
prev_commit=''
prev_image_digest=''
metadata_assignments=0

while IFS= read -r line || [[ -n $line ]]; do
  [[ $line =~ ^[[:space:]]*($|#) ]] && continue
  if [[ $line =~ ^OLP_PREVIOUS_RELEASED_VERSION=(.+)$ ]]; then
    [[ -z $prev_version ]] || {
      echo "duplicate OLP_PREVIOUS_RELEASED_VERSION assignment in $metadata_file" >&2
      exit 1
    }
    prev_version=${BASH_REMATCH[1]}
    ((metadata_assignments += 1))
    continue
  fi
  if [[ $line =~ ^OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION=(.+)$ ]]; then
    [[ -z $prev_migration ]] || {
      echo "duplicate OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION assignment in $metadata_file" >&2
      exit 1
    }
    prev_migration=${BASH_REMATCH[1]}
    ((metadata_assignments += 1))
    continue
  fi
  if [[ $line =~ ^OLP_PREVIOUS_RELEASED_COMMIT=(.+)$ ]]; then
    [[ -z $prev_commit ]] || {
      echo "duplicate OLP_PREVIOUS_RELEASED_COMMIT assignment in $metadata_file" >&2
      exit 1
    }
    prev_commit=${BASH_REMATCH[1]}
    ((metadata_assignments += 1))
    continue
  fi
  if [[ $line =~ ^OLP_PREVIOUS_RELEASED_IMAGE_DIGEST=(.+)$ ]]; then
    [[ -z $prev_image_digest ]] || {
      echo "duplicate OLP_PREVIOUS_RELEASED_IMAGE_DIGEST assignment in $metadata_file" >&2
      exit 1
    }
    prev_image_digest=${BASH_REMATCH[1]}
    ((metadata_assignments += 1))
    continue
  fi
  echo "release metadata contains an unsupported line: $line" >&2
  exit 1
done <"$metadata_file"

(( metadata_assignments == 4 )) || {
  echo "release metadata must contain all 4 required assignments (OLP_PREVIOUS_RELEASED_VERSION, OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION, OLP_PREVIOUS_RELEASED_COMMIT, OLP_PREVIOUS_RELEASED_IMAGE_DIGEST)" >&2
  exit 1
}

[[ $prev_version =~ $semver ]] || {
  echo "previous released version is not semantic: $prev_version" >&2
  exit 1
}

[[ $prev_version != "$workspace_version" ]] || {
  echo "previous released version $prev_version cannot match candidate workspace version $workspace_version" >&2
  exit 1
}

[[ $prev_migration =~ ^[0-9]{4}$ ]] || {
  echo "previous released schema migration must be 4 digits: $prev_migration" >&2
  exit 1
}

[[ $prev_commit =~ ^[0-9a-f]{40}$ ]] || {
  echo "previous released commit must be a 40-character hex SHA: $prev_commit" >&2
  exit 1
}

[[ $prev_image_digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "previous released image digest must be sha256:<64-hex>: $prev_image_digest" >&2
  exit 1
}

if [[ -d "$root/crates/olp-db/migrations" ]]; then
  shopt -s nullglob
  matching_migrations=("$root/crates/olp-db/migrations/${prev_migration}_"*.sql)
  shopt -u nullglob
  (( ${#matching_migrations[@]} == 1 )) || {
    echo "previous released schema migration $prev_migration does not match a unique tracked migration file" >&2
    exit 1
  }
fi

echo "release metadata is consistent at $workspace_version (previous release: $prev_version, schema migration: $prev_migration)"
