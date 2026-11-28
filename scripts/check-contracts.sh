#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
(( $# == 0 )) || { echo "Usage: $0" >&2; exit 2; }
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
./scripts/api.sh "$scratch"
while IFS= read -r -d '' generated; do
  if ! cmp -s -- "$generated" "${generated#"$scratch"/}"; then
    printf '%s is stale, edited by hand or missing. Change its source and run make api\n' "${generated#"$scratch"/}" >&2
    exit 1
  fi
done < <(find "$scratch" -type f -print0)
echo 'Generated contracts are current'
