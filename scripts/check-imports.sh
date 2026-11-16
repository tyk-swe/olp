#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
recorded=scripts/project-imports.txt
module=github.com/tyk-swe/olp

case ${1:-check} in
  check | update) ;;
  *)
    echo "usage: $0 [check|update]" >&2
    exit 2
    ;;
esac

actual=$(mktemp)
trap 'rm -f "$actual"' EXIT
{
  echo "# Every production import edge between project Go packages, one edge per line."
  echo "# Test-only and build-tagged test imports are not recorded."
  echo "# Regenerate with ./scripts/check-imports.sh update after reviewing a new edge."
  go list -f '{{.ImportPath}} {{join .Imports " "}}' ./... |
    while read -r package imports; do
      [[ $package == "$module"/* ]] || continue
      for imported in $imports; do
        [[ $imported == "$module"/* ]] || continue
        printf '%s %s\n' "${package#"$module/"}" "${imported#"$module/"}"
      done
    done | LC_ALL=C sort -u
} >"$actual"

if [[ ${1:-check} == update ]]; then
  cp "$actual" "$recorded"
  echo "recorded $(grep -c -v '^#' "$recorded") project import edges in $recorded"
  exit 0
fi

if [[ ! -f $recorded ]]; then
  echo "check-imports: $recorded is missing, run ./scripts/check-imports.sh update" >&2
  exit 1
fi
if ! diff -u "$recorded" "$actual"; then
  echo "check-imports: the project import graph changed." >&2
  echo "Review the changed edges above, then run ./scripts/check-imports.sh update and commit $recorded." >&2
  exit 1
fi
