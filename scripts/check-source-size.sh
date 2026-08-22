#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repository_root=${1:-$(cd "$script_dir/.." && pwd)}
[[ $# -le 1 ]] || { echo "usage: $0 [REPOSITORY_ROOT]" >&2; exit 2; }
repository_root=$(cd "$repository_root" && pwd -P)

shopt -s nullglob
application_roots=("$repository_root"/apps/*/src)
crate_roots=("$repository_root"/crates/*/src)
shopt -u nullglob
console_root="$repository_root/console/src"
if (( ${#application_roots[@]} == 0 || ${#crate_roots[@]} == 0 )) || [[ ! -d $console_root ]]; then
  echo "production source roots are missing" >&2
  exit 2
fi
source_roots=("${application_roots[@]}" "${crate_roots[@]}" "$console_root")

readonly maximum_bytes=30000
readonly generated_schema="$repository_root/console/src/lib/api/schema.d.ts"
scanned_files=$(find "${source_roots[@]}" -type f ! -path "$generated_schema" -printf . | wc -c)
(( scanned_files > 0 )) || { echo "no production source files were found" >&2; exit 2; }

listing=$(mktemp)
trap 'rm -f "$listing"' EXIT
find "${source_roots[@]}" -type f ! -path "$generated_schema" -size +${maximum_bytes}c \
  -print0 > "$listing"
mapfile -d '' -t oversized_files < "$listing"

if (( ${#oversized_files[@]} > 0 )); then
  echo "production source files exceed the ${maximum_bytes}-byte limit:" >&2
  for source_file in "${oversized_files[@]}"; do
    printf '  %s (%s bytes)\n' "${source_file#"$repository_root"/}" "$(stat -c %s "$source_file")" >&2
  done
  exit 1
fi

printf 'source-size policy is clean (%d files, limit %d bytes)\n' \
  "$scanned_files" "$maximum_bytes"
