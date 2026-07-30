#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
workspace_root=$(cd "$script_dir/.." && pwd)
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"
cd "$workspace_root"

for required_executable in rg awk dirname; do
  validation_require_executable "$required_executable"
done

storage_root=crates/storage/src
validation_require_directory "$storage_root"

forbidden_row_patterns=(
  '\bPgRow\b'
  'sqlx::Row'
  'use sqlx::\{[^}]*\bRow\b'
  '\.try_get(?:::<[^>]+>)?\s*\(\s*"'
  '\.get::<[^>]+,\s*_>\s*\(\s*"'
)

# One rg pass over the tree with every pattern; -e flags OR-combine.
forbidden_pattern_args=()
for pattern in "${forbidden_row_patterns[@]}"; do
  forbidden_pattern_args+=(-e "$pattern")
done
forbidden_rows=
forbidden_rows_matched=
checked_rg_capture forbidden_rows forbidden_rows_matched \
  "scan forbidden SQLx row decoding" "$storage_root" \
  --pcre2 --line-number --glob '*.rs' "${forbidden_pattern_args[@]}" "$storage_root"
if (( forbidden_rows_matched )); then
  printf '%s\n' "$forbidden_rows"
  echo "production storage must decode SQL rows through checked records or typed FromRow models" >&2
  exit 1
fi

runtime_query_output=
runtime_queries_matched=
checked_rg_capture runtime_query_output runtime_queries_matched \
  "scan runtime SQL APIs" "$storage_root" \
  --line-number --glob '*.rs' 'sqlx::(query|query_as|query_scalar)\s*\(' "$storage_root"
if (( runtime_queries_matched )); then
  printf '%s\n' "$runtime_query_output"
  echo "unexpected runtime SQL API usage in production storage" >&2
  exit 1
fi

checked_query_counts=
checked_query_counts_matched=
checked_rg_capture checked_query_counts checked_query_counts_matched \
  "count checked SQLx queries" "$storage_root" \
  --count-matches --glob '*.rs' \
  'sqlx::(query|query_as|query_scalar)!\s*\(' "$storage_root"
checked_queries=0
if (( checked_query_counts_matched )); then
  checked_queries=$(awk -F: '{ total += $NF } END { print total + 0 }' <<< "$checked_query_counts")
fi

typed_row_counts=
typed_row_counts_matched=
checked_rg_capture typed_row_counts typed_row_counts_matched \
  "count typed SQLx rows" "$storage_root" \
  --count-matches --glob '*.rs' \
  '(derive\([^)]*FromRow|derive\([^)]*sqlx::FromRow)' "$storage_root"
typed_rows=0
if (( typed_row_counts_matched )); then
  typed_rows=$(awk -F: '{ total += $NF } END { print total + 0 }' <<< "$typed_row_counts")
fi
if (( checked_queries == 0 || typed_rows == 0 )); then
  echo "checked query or typed dynamic-row coverage unexpectedly disappeared" >&2
  exit 1
fi

printf 'storage SQLx policy is clean (%d checked queries, %d typed row models)\n' \
  "$checked_queries" "$typed_rows"
