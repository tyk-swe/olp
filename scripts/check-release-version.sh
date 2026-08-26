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

for required_executable in rg awk sed sort dirname jq; do
  validation_require_executable "$required_executable"
done
for required_directory in "$root/console" "$root/deploy" "$root/deploy/helm"; do
  validation_require_directory "$required_directory"
done
for required_file in \
  "$root/Cargo.toml" "$root/console/package.json" \
  "$root/deploy/helm/Chart.yaml" "$root/deploy/Dockerfile" \
  "$root/rust-toolchain.toml" "$root/.github/actions/setup-rust/action.yml" \
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

released_migration=$(sed -nE 's/^OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION=([0-9]{4})$/\1/p' "$root/release-metadata.env")
[[ -n $released_migration ]] || {
  echo "release-metadata.env does not pin a four-digit OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION" >&2
  exit 1
}
ls "$root/crates/olp-db/migrations/${released_migration}_"*.sql >/dev/null 2>&1 || {
  echo "release-metadata.env pins migration $released_migration, which is not a tracked migration" >&2
  exit 1
}

toolchain_rust=$(sed -nE 's/^channel = "([^"]+)"$/\1/p' "$root/rust-toolchain.toml")
action_rust=$(sed -nE 's/^[[:space:]]*toolchain: "([^"]+)"$/\1/p' "$root/.github/actions/setup-rust/action.yml")
image_rust=$(sed -nE 's|^FROM rust:([^-]+)-bookworm@sha256:[0-9a-f]{64}.*$|\1|p' "$root/deploy/Dockerfile" | sort -u)

[[ $toolchain_rust =~ $semver ]] || {
  echo "rust-toolchain.toml channel is not a pinned version: ${toolchain_rust:-unset}" >&2
  exit 1
}
for pair in \
  ".github/actions/setup-rust toolchain:$action_rust" \
  "deploy/Dockerfile rust base image:$image_rust"; do
  label=${pair%%:*}
  value=${pair#*:}
  [[ $value == "$toolchain_rust" ]] || {
    echo "$label is ${value:-unset}, expected $toolchain_rust" >&2
    exit 1
  }
done

echo "release metadata is consistent at $workspace_version (Rust $toolchain_rust)"
