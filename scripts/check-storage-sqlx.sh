#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
workspace_root=$(cd "$script_dir/.." && pwd)
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"
cd "$workspace_root"

validation_require_executable rg

storage_root=crates/olp-db/src
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

echo "storage SQLx policy is clean"
