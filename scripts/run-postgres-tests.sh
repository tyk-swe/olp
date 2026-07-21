#!/usr/bin/env bash
set -euo pipefail

: "${OLP_TEST_DATABASE_ADMIN_URL:?set OLP_TEST_DATABASE_ADMIN_URL to the PostgreSQL maintenance database}"
: "${OLP_TEST_DATABASE_URL_PREFIX:?set OLP_TEST_DATABASE_URL_PREFIX without a trailing database name}"
owner=${OLP_TEST_DATABASE_OWNER:-olp}
timeout_seconds=${OLP_POSTGRES_TEST_TIMEOUT_SECONDS:-900}

if [[ ! $timeout_seconds =~ ^[1-9][0-9]*$ ]]; then
  echo "OLP_POSTGRES_TEST_TIMEOUT_SECONDS must be a positive integer" >&2
  exit 64
fi

for command in createdb dropdb cargo sha256sum timeout; do
  command -v "$command" >/dev/null 2>&1 || {
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

run_token=${GITHUB_RUN_ID:-local_${PPID}_$$_${RANDOM}}
run_token+=_${GITHUB_RUN_ATTEMPT:-0}
run_token=${run_token,,}
run_token=${run_token//[^a-z0-9_]/_}
run_token=${run_token:0:24}
current_database=

cleanup_database() {
  if [[ -z $current_database ]]; then
    return 0
  fi
  local database=$current_database
  if ! dropdb --maintenance-db="$OLP_TEST_DATABASE_ADMIN_URL" \
    --if-exists --force "$database"; then
    return 1
  fi
  current_database=
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if ! cleanup_database; then
    echo "failed to remove PostgreSQL integration database" >&2
    (( status == 0 )) && status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

matched=0
for entry in "${tests[@]}"; do
  if [[ -n ${OLP_POSTGRES_TEST_FILTER:-} && $entry != *"$OLP_POSTGRES_TEST_FILTER"* ]]; then
    continue
  fi

  package=${entry%%:*}
  test_name=${entry#*:}
  test_hash=$(printf '%s' "$entry" | sha256sum)
  test_hash=${test_hash%% *}
  database="olp_test_${run_token}_${matched}_${test_hash:0:10}"
  if [[ ! $database =~ ^[a-z0-9_]+$ || ${#database} -gt 63 ]]; then
    echo "unsafe generated database name: $database" >&2
    exit 1
  fi

  # Defensively clear a colliding name before creation. Register the name
  # before createdb so the EXIT trap also handles partial creation failures.
  dropdb --maintenance-db="$OLP_TEST_DATABASE_ADMIN_URL" \
    --if-exists --force "$database"
  current_database=$database
  createdb --maintenance-db="$OLP_TEST_DATABASE_ADMIN_URL" \
    --owner="$owner" "$database"

  echo "running ${package}/${test_name} against isolated ${database}"
  (
    unset OLP_DATABASE_URL
    export OLP_TEST_DATABASE_URL="${OLP_TEST_DATABASE_URL_PREFIX%/}/${database}"
    timeout --kill-after=30s "${timeout_seconds}s" \
      cargo test --locked -p "$package" --test "$test_name" -- \
        --include-ignored --test-threads=1
  )

  cleanup_database
  matched=$((matched + 1))
done

if (( matched == 0 )); then
  echo "no PostgreSQL integration tests matched OLP_POSTGRES_TEST_FILTER=${OLP_POSTGRES_TEST_FILTER:-}" >&2
  exit 1
fi
