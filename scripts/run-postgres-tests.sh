#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/lib/postgres-test-databases.sh
source "$script_dir/lib/postgres-test-databases.sh"

# Runs the #[ignore]d PostgreSQL/Valkey integration suites through nextest
# (profile "db" in .config/nextest.toml, which bounds parallelism and
# per-test timeouts). Every test provisions its own uniquely named database
# via olp_db::test_support::TestDb and drops it on completion; this
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

OLP_DB_TEST_RUNNER=${OLP_DB_TEST_RUNNER:-cargo nextest run}
read -r -a db_test_runner <<< "$OLP_DB_TEST_RUNNER"
if ((${#db_test_runner[@]} == 0)); then
  echo "OLP_DB_TEST_RUNNER must name a command" >&2
  exit 1
fi

for command in psql sha256sum timeout "${db_test_runner[0]}"; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

# TestDb embeds this token in every database name (olp_test_{token}_...),
# scoping the sweep below to databases this run created. Hashing the full
# run identity keeps tokens collision-resistant — a truncated GITHUB_RUN_ID
# would share its prefix across nearby runs.
run_token=$(postgres_test_run_token)
export OLP_TEST_RUN_TOKEN="$run_token"

sweep_leftover_databases() {
  postgres_test_sweep_databases \
    "$OLP_TEST_DATABASE_ADMIN_URL" "olp_test_${run_token}_" lower-identifier integration
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

sweep_leftover_databases

skip_args=()
if [[ -z ${OLP_VALKEY_URL:-} ]]; then
  # `--skip` lives in nextest's emulated libtest section (after `--`) and
  # composes with any caller filterset or name filter, unlike a second -E
  # expression, which nextest would OR-combine. Reuse the caller's `--`
  # section if they already opened one.
  echo "OLP_VALKEY_URL is unset; skipping the Valkey-backed suites" >&2
  skip_args=(-- --skip distributed_limits_valkey --skip request_metadata_consumer_valkey)
  for argument in "$@"; do
    if [[ $argument == -- ]]; then
      skip_args=(--skip distributed_limits_valkey --skip request_metadata_consumer_valkey)
      break
    fi
  done
fi

(
  unset OLP_DATABASE_URL
  SQLX_OFFLINE=true "${db_test_runner[@]}" --locked --all-features \
    --package olp-db --package olp \
    --profile db --run-ignored ignored-only \
    "$@" "${skip_args[@]}"
)
