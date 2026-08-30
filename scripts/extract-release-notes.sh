#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 || ${1:-} == --help || ${1:-} == -h ]]; then
  echo "usage: $0 TAG CHANGELOG OUTPUT" >&2
  [[ $# -eq 1 ]] && exit 0 || exit 2
fi

tag=$1
changelog=$2
output=$3
[[ $tag =~ ^v(0|[1-9][0-9]*)\.([0-9]+)\.([0-9]+)$ ]] || {
  echo "release tag must match vMAJOR.MINOR.PATCH: $tag" >&2
  exit 1
}
[[ -f $changelog ]] || { echo "changelog is missing: $changelog" >&2; exit 1; }

version=${tag#v}
heading_prefix="## [$version] - "
version_pattern=${version//./\\.}
heading_count=$(grep -Ec \
  "^## \\[$version_pattern\\] - [0-9]{4}-[0-9]{2}-[0-9]{2}$" "$changelog" || true)
[[ $heading_count == 1 ]] || {
  echo "changelog must contain exactly one dated $version section" >&2
  exit 1
}

raw=$(mktemp)
trap 'rm -f "$raw"' EXIT
awk -v prefix="$heading_prefix" '
  index($0, prefix) == 1 { capture = 1; next }
  capture && /^## \[/ { exit }
  capture { print }
' "$changelog" > "$raw"
awk '
  { lines[NR] = $0 }
  $0 !~ /^[[:space:]]*$/ { if (!first) first = NR; last = NR }
  END { for (line = first; line <= last; line++) print lines[line] }
' "$raw" > "$output"
grep -q '[^[:space:]]' "$output" || {
  echo "changelog section for $version is empty" >&2
  exit 1
}
