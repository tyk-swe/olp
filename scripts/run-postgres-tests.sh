#!/usr/bin/env bash
set -euo pipefail

: "${OLP_TEST_DATABASE_ADMIN_URL:?set OLP_TEST_DATABASE_ADMIN_URL to the PostgreSQL maintenance database}"
: "${OLP_TEST_DATABASE_URL_PREFIX:?set OLP_TEST_DATABASE_URL_PREFIX without a trailing database name}"
owner=${OLP_TEST_DATABASE_OWNER:-olp}

for command in psql createdb cargo; do
  command -v "$command" >/dev/null || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

test_roots=(
  "olp-storage:crates/storage/tests"
  "olp:apps/olp/tests"
)
tests=()
for root in "${test_roots[@]}"; do
  package=${root%%:*}
  directory=${root#*:}
  [[ -d $directory ]] || {
    echo "PostgreSQL integration test directory is missing: $directory" >&2
    exit 1
  }
  while IFS= read -r -d '' test_file; do
    test_name=${test_file##*/}
    tests+=("$package:${test_name%.rs}")
  done < <(find "$directory" -maxdepth 1 -type f -name '*_postgres.rs' -print0 | LC_ALL=C sort -z)
done

if (( ${#tests[@]} == 0 )); then
  echo "no PostgreSQL integration tests were discovered" >&2
  exit 1
fi

index=0
for entry in "${tests[@]}"; do
  if [[ -n ${OLP_POSTGRES_TEST_FILTER:-} && $entry != *"$OLP_POSTGRES_TEST_FILTER"* ]]; then
    continue
  fi
  package=${entry%%:*}
  test_name=${entry#*:}
  database="olp_test_${index}_${test_name}"
  if [[ ! $database =~ ^[a-z0-9_]+$ ]]; then
    echo "unsafe generated database name: $database" >&2
    exit 1
  fi
  psql "$OLP_TEST_DATABASE_ADMIN_URL" -X --no-psqlrc -v ON_ERROR_STOP=1 \
    --command="DROP DATABASE IF EXISTS ${database} WITH (FORCE)"
  createdb --maintenance-db="$OLP_TEST_DATABASE_ADMIN_URL" --owner="$owner" "$database"
  echo "running ${package}/${test_name} against isolated ${database}"
  OLP_TEST_DATABASE_URL="${OLP_TEST_DATABASE_URL_PREFIX%/}/${database}" \
    cargo test --locked -p "$package" --test "$test_name" -- \
      --ignored --test-threads=1
  index=$((index + 1))
done

if (( index == 0 )); then
  echo "no PostgreSQL integration tests matched OLP_POSTGRES_TEST_FILTER=${OLP_POSTGRES_TEST_FILTER:-}" >&2
  exit 1
fi
