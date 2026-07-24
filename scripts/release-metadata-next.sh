#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 VERSION IMAGE_DIGEST [OUTPUT]" >&2
}

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  usage
  exit 0
fi
[[ $# -ge 2 && $# -le 3 ]] || {
  usage
  exit 2
}

version=$1
image=$2
output=${3:-release-metadata.next.env}
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
[[ $version =~ $semver ]] || { echo "VERSION must be semantic" >&2; exit 2; }
[[ $image =~ ^ghcr\.io/tyk-swe/olp@sha256:[0-9a-f]{64}$ ]] || {
  echo "IMAGE_DIGEST must be ghcr.io/tyk-swe/olp@sha256:<64 lowercase hex>" >&2
  exit 2
}
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mapfile -t migrations < <(find "$root/crates/storage/migrations" -maxdepth 1 -type f \
  -name '[0-9][0-9][0-9][0-9]_*.sql' -printf '%f\n' | LC_ALL=C sort)
(( ${#migrations[@]} > 0 )) || { echo "no migrations found" >&2; exit 1; }
latest=${migrations[${#migrations[@]}-1]%%_*}

[[ ! -e $output ]] || { echo "refusing to overwrite $output" >&2; exit 1; }
umask 077
{
  echo "# Generated release rollover metadata; commit this as release-metadata.env."
  echo "OLP_PREVIOUS_RELEASE_MODE=released"
  echo "OLP_PREVIOUS_RELEASED_VERSION=$version"
  echo "OLP_PREVIOUS_RELEASED_IMAGE=$image"
  echo "OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION=$latest"
} > "$output"
echo "$output"
