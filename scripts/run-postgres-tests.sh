#!/usr/bin/env bash
set -euo pipefail

# Runs the #[ignore]d PostgreSQL/Valkey integration suites through nextest
# (profile "db" in .config/nextest.toml, which bounds parallelism and
# per-test timeouts). Every test provisions its own uniquely named database
# via olp_storage::test_support::TestDb and drops it on completion; this
# script only sweeps leftovers from workers that were killed mid-test.
#
# Databases are namespaced by a per-run token so the sweep never touches a
# concurrent invocation's databases. If this script itself dies without
# running its EXIT trap, remove abandoned databases manually:
#   psql "$OLP_TEST_DATABASE_ADMIN_URL" -Atc \
#     "SELECT datname FROM pg_database WHERE datname LIKE 'olp\_test\_%'"
#
# Extra arguments pass through to `cargo nextest run`, e.g.:
#   ./scripts/run-postgres-tests.sh -E 'test(upgrade_0021)'

: "${OLP_TEST_DATABASE_ADMIN_URL:?set OLP_TEST_DATABASE_ADMIN_URL to the PostgreSQL maintenance database}"
: "${OLP_TEST_DATABASE_URL_PREFIX:?set OLP_TEST_DATABASE_URL_PREFIX without a trailing database name}"

for command in psql cargo cargo-nextest sha256sum timeout; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

# TestDb embeds this token in every database name (olp_test_{token}_...),
# scoping the sweep below to databases this run created. Hashing the full
# run identity keeps tokens collision-resistant — a truncated GITHUB_RUN_ID
# would share its prefix across nearby runs.
run_token=$(printf '%s' "${GITHUB_RUN_ID:-}_${GITHUB_RUN_ATTEMPT:-}_$$_${RANDOM}_$(date +%s%N)" | sha256sum)
run_token=${run_token:0:10}
export OLP_TEST_RUN_TOKEN="$run_token"

# Explicit `return 1`s throughout: this runs under `if !`, where set -e is
# suspended, so unchecked psql failures would otherwise read as success.
sweep_leftover_databases() {
  local leftovers database
  # The server-side regex is string-anchored (^/$ do not match around
  # embedded newlines in PostgreSQL), so a hostile quoted identifier like
  # "olp_test_x\nproduction" can never smuggle a second line into this
  # newline-framed output; the per-line check below is a second layer.
  # Bounded psql calls: a stalled server must fail the sweep loudly instead
  # of hanging the run (or its EXIT trap) until the job timeout.
  if ! leftovers=$(timeout --kill-after=5s 30s \
    psql "$OLP_TEST_DATABASE_ADMIN_URL" --no-psqlrc --tuples-only --no-align \
    --command "SELECT datname FROM pg_database
               WHERE datname ~ '^olp_test_${run_token}_[a-z0-9_]+$'"); then
    echo "failed to list integration databases" >&2
    return 1
  fi
  while IFS= read -r database; do
    [[ -n $database ]] || continue
    if [[ ! $database =~ ^[a-z0-9_]+$ ]]; then
      echo "refusing to drop suspicious database name: $database" >&2
      return 1
    fi
    echo "dropping leftover integration database $database"
    timeout --kill-after=5s 30s \
      psql "$OLP_TEST_DATABASE_ADMIN_URL" --no-psqlrc --quiet \
      --command "DROP DATABASE IF EXISTS \"$database\" WITH (FORCE)" >/dev/null || return 1
  done <<<"$leftovers"
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if ! sweep_leftover_databases; then
    echo "failed to sweep leftover integration databases" >&2
    ((status == 0)) && status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

skip_args=()
if [[ -z ${OLP_VALKEY_URL:-} ]]; then
  # `--skip` lives in nextest's emulated libtest section (after `--`) and
  # composes with any caller filterset or name filter, unlike a second -E
  # expression, which nextest would OR-combine. Reuse the caller's `--`
  # section if they already opened one.
  echo "OLP_VALKEY_URL is unset; skipping the distributed_limits_valkey suite" >&2
  skip_args=(-- --skip distributed_limits_valkey)
  for argument in "$@"; do
    if [[ $argument == -- ]]; then
      skip_args=(--skip distributed_limits_valkey)
      break
    fi
  done
fi

(
  unset OLP_DATABASE_URL
  SQLX_OFFLINE=true cargo nextest run --locked --all-features \
    --package olp-storage --package olp \
    --profile db --run-ignored ignored-only \
    "$@" "${skip_args[@]}"
)
