#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repository_root=${1:-$(cd "$script_dir/.." && pwd)}
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"

if [[ $# -gt 1 ]]; then
  echo "usage: $0 [REPOSITORY_ROOT]" >&2
  exit 2
fi

for required_executable in find sort stat; do
  validation_require_executable "$required_executable"
done
validation_require_directory "$repository_root"
repository_root=$(cd "$repository_root" && pwd -P)
validation_require_directory "$repository_root/apps"
validation_require_directory "$repository_root/crates"
validation_require_directory "$repository_root/console/src"

shopt -s nullglob
application_roots=("$repository_root"/apps/*/src)
crate_roots=("$repository_root"/crates/*/src)
shopt -u nullglob
(( ${#application_roots[@]} > 0 )) || {
  echo "$(validation_script_name): no application source roots matched apps/*/src" >&2
  exit 2
}
(( ${#crate_roots[@]} > 0 )) || {
  echo "$(validation_script_name): no crate source roots matched crates/*/src" >&2
  exit 2
}

source_roots=(
  "${application_roots[@]}"
  "${crate_roots[@]}"
  "$repository_root/console/src"
)
for source_root in "${source_roots[@]}"; do
  validation_require_directory "$source_root"
done

readonly maximum_bytes=30000
readonly generated_schema="$repository_root/console/src/lib/api/schema.d.ts"
scanned_files=0
oversized_files=()

shopt -s lastpipe
if ! find "${source_roots[@]}" -type f -print0 \
  | sort -z \
  | while IFS= read -r -d '' source_file; do
      [[ $source_file == "$generated_schema" ]] && continue
      if ! size=$(stat -c '%s' -- "$source_file"); then
        printf '%s: failed to read source size: %s\n' \
          "$(validation_script_name)" "$source_file" >&2
        exit 2
      fi
      [[ $size =~ ^[0-9]+$ ]] || {
        printf '%s: invalid source size for %s: %s\n' \
          "$(validation_script_name)" "$source_file" "$size" >&2
        exit 2
      }
      scanned_files=$((scanned_files + 1))
      if (( size > maximum_bytes )); then
        oversized_files+=("${source_file#"$repository_root"/}:$size")
      fi
    done; then
  echo "$(validation_script_name): source file discovery failed" >&2
  exit 2
fi
shopt -u lastpipe

(( scanned_files > 0 )) || {
  echo "$(validation_script_name): no production source files were found" >&2
  exit 2
}
if (( ${#oversized_files[@]} > 0 )); then
  echo "production source files exceed the ${maximum_bytes}-byte limit:" >&2
  for violation in "${oversized_files[@]}"; do
    path=${violation%:*}
    size=${violation##*:}
    printf '  %s (%s bytes)\n' "$path" "$size" >&2
  done
  exit 1
fi

printf 'source-size policy is clean (%d files, limit %d bytes)\n' \
  "$scanned_files" "$maximum_bytes"
