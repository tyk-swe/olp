#!/usr/bin/env bash
# Enforces the AGENTS.md size rules: source files stay under 30 KB and Rust
# functions under 100 lines. Existing violations are grandfathered in
# scripts/source-size-baseline.txt; that list may only shrink. `--update`
# rewrites the baseline from the current tree and exists for retiring entries.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$script_dir/.." && pwd)
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"
cd "$root"

for required_executable in awk find sort comm wc; do
  validation_require_executable "$required_executable"
done

baseline=${SOURCE_SIZE_BASELINE:-scripts/source-size-baseline.txt}
max_file_bytes=${SOURCE_SIZE_MAX_FILE_BYTES:-30720}
max_fn_lines=${SOURCE_SIZE_MAX_FN_LINES:-100}
roots=()
for candidate in apps crates console/src tests; do
  [[ -d $candidate ]] && roots+=("$candidate")
done
update=0
[[ ${1:-} == --update ]] && update=1

# Rust functions outside test code. Test modules are skipped by watching for
# `#[cfg(test)]` and ignoring the item that follows it; dedicated test files
# are skipped by path. Brace depth is counted after stripping string literals
# and line comments, which is exact enough for rustfmt-formatted code.
long_functions() {
  awk -v max="$max_fn_lines" '
    function strip(line) {
      gsub(/"([^"\\]|\\.)*"/, "\"\"", line)
      gsub(/\047([^\047\\]|\\.)*\047/, "", line)
      sub(/\/\/.*$/, "", line)
      return line
    }
    function braces(line,   opened, closed) {
      opened = gsub(/\{/, "{", line)
      closed = gsub(/\}/, "}", line)
      return opened - closed
    }
    FNR == 1 { depth = 0; skipping = 0; skip_depth = 0; in_fn = 0; pending_skip = 0 }
    {
      line = strip($0)
      if (skipping) {
        skip_depth += braces(line)
        if (skip_depth <= 0 && line ~ /\}/) skipping = 0
        next
      }
      if (line ~ /^[[:space:]]*#\[cfg\(test\)\]/) { pending_skip = 1; next }
      if (pending_skip && line ~ /^[[:space:]]*(pub(\([^)]*\))?[[:space:]]+)?mod[[:space:]]/) {
        pending_skip = 0
        if (line ~ /\{/) { skipping = 1; skip_depth = braces(line); if (skip_depth <= 0) skipping = 0 }
        next
      }
      if (line !~ /^[[:space:]]*#\[/ && line !~ /^[[:space:]]*$/) pending_skip = 0
      if (!in_fn && match(line, /(^|[[:space:]])fn[[:space:]]+[A-Za-z_][A-Za-z0-9_]*/)) {
        name = substr(line, RSTART, RLENGTH)
        sub(/.*fn[[:space:]]+/, "", name)
        in_fn = 1; fn_start = FNR; fn_depth = 0; seen_brace = 0
      }
      if (in_fn) {
        delta = braces(line)
        if (line ~ /\{/) seen_brace = 1
        fn_depth += delta
        if (!seen_brace && line ~ /;[[:space:]]*$/) { in_fn = 0 }
        else if (seen_brace && fn_depth <= 0) {
          length_lines = FNR - fn_start + 1
          if (length_lines > max) printf "fn:%s:%s:%d\n", FILENAME, name, length_lines
          in_fn = 0
        }
      }
    }
  ' "$@"
}

rust_sources=$(find "${roots[@]}" -type f -name '*.rs' \
  -not -path '*/target/*' -not -path '*/node_modules/*' \
  -not -name 'tests.rs' -not -path '*/tests/*' -not -path '*/test_support*' | sort)
large_files=$(find "${roots[@]}" -type f \( -name '*.rs' -o -name '*.ts' -o -name '*.svelte' \) \
  -not -path '*/target/*' -not -path '*/node_modules/*' -not -path '*/.svelte-kit/*' \
  -not -name 'schema.d.ts' -size +"$((max_file_bytes / 1024))"k | sort)

violations=$(
  {
    for file in $large_files; do printf 'file:%s\n' "$file"; done
    # shellcheck disable=SC2086
    [[ -z $rust_sources ]] || long_functions $rust_sources | awk -F: '{ print $1 ":" $2 ":" $3 }'
  } | sort -u
)

if (( update )); then
  printf '%s\n' "$violations" | sed '/^$/d' > "$baseline"
  echo "wrote $(wc -l < "$baseline") grandfathered entries to $baseline"
  exit 0
fi

validation_require_file "$baseline"
known=$(sort -u "$baseline")
new=$(comm -23 <(printf '%s\n' "$violations" | sed '/^$/d') <(printf '%s\n' "$known" | sed '/^$/d'))
stale=$(comm -13 <(printf '%s\n' "$violations" | sed '/^$/d') <(printf '%s\n' "$known" | sed '/^$/d'))

status=0
if [[ -n $new ]]; then
  echo "source size rule broken (files > ${max_file_bytes} bytes, fns > ${max_fn_lines} lines):" >&2
  printf '  %s\n' "$new" >&2
  status=1
fi
if [[ -n $stale ]]; then
  echo "fixed entries still listed in $baseline; remove them:" >&2
  printf '  %s\n' "$stale" >&2
  status=1
fi
(( status )) && exit "$status"
echo "source sizes are within the rules ($(printf '%s\n' "$known" | sed '/^$/d' | wc -l) grandfathered)"
