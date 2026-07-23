#!/usr/bin/env bash
set -euo pipefail

storage_root=crates/storage/src

forbidden_row_patterns=(
  '\bPgRow\b'
  'sqlx::Row'
  'use sqlx::\{[^}]*\bRow\b'
  '\.try_get(?:::<[^>]+>)?\s*\(\s*"'
  '\.get::<[^>]+,\s*_>\s*\(\s*"'
)

for pattern in "${forbidden_row_patterns[@]}"; do
  if rg --pcre2 --line-number --glob '*.rs' "$pattern" "$storage_root"; then
    echo "production storage must decode SQL rows through checked records or typed FromRow models" >&2
    exit 1
  fi
done

# Runtime SQL construction remains available for SQL whose identity is truly
# dynamic, but each production use must be reviewed and entered here with its
# file/line pattern. The current storage implementation needs no exceptions.
approved_runtime_queries=()
mapfile -t runtime_queries < <(
  rg --line-number --glob '*.rs' 'sqlx::(query|query_as|query_scalar)\s*\(' "$storage_root" || true
)
for runtime_query in "${runtime_queries[@]}"; do
  approved=false
  for pattern in "${approved_runtime_queries[@]}"; do
    if [[ $runtime_query =~ $pattern ]]; then
      approved=true
      break
    fi
  done
  if [[ $approved != true ]]; then
    printf '%s\n' "$runtime_query"
    echo "unexpected runtime SQL API usage in production storage" >&2
    exit 1
  fi
done

checked_queries=$(rg --count-matches --glob '*.rs' \
  'sqlx::(query|query_as|query_scalar)!\s*\(' "$storage_root" \
  | awk -F: '{ total += $2 } END { print total + 0 }')
typed_rows=$(rg --count-matches --glob '*.rs' \
  '(derive\([^)]*FromRow|derive\([^)]*sqlx::FromRow)' "$storage_root" \
  | awk -F: '{ total += $2 } END { print total + 0 }')
if (( checked_queries == 0 || typed_rows == 0 )); then
  echo "checked query or typed dynamic-row coverage unexpectedly disappeared" >&2
  exit 1
fi

printf 'storage SQLx policy is clean (%d checked queries, %d typed row models)\n' \
  "$checked_queries" "$typed_rows"
