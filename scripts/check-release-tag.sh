#!/usr/bin/env bash
set -euo pipefail

if [[ $# -gt 1 || ${1:-} == --help || ${1:-} == -h ]]; then
  echo "usage: RELEASE_TAG=v2.2.0 $0 [TAG]" >&2
  [[ $# -eq 1 ]] && exit 0 || exit 2
fi

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tag=${1:-${RELEASE_TAG:-}}
if [[ -z $tag && ${GITHUB_REF_TYPE:-} == tag ]]; then
  tag=${GITHUB_REF_NAME:-}
fi

if [[ -z $tag && ${DRY_RUN:-false} == true ]]; then
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
  ' "$script_dir/../Cargo.toml")
  tag="v$workspace_version"
fi

[[ -n $tag ]] || {
  echo "release tag is required" >&2
  exit 1
}
[[ $tag =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
  echo "release tag must match vMAJOR.MINOR.PATCH: $tag" >&2
  exit 1
}
if [[ ${GITHUB_EVENT_NAME:-} == workflow_dispatch && ${DRY_RUN:-false} != true ]]; then
  echo "workflow_dispatch may only run with dry_run=true; publish from a matching tag" >&2
  exit 1
fi

"$script_dir/check-release-version.sh" "${tag#v}"
echo "release tag matches repository metadata: $tag"
