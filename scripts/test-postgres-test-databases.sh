#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=scripts/lib/postgres-test-databases.sh
source "$script_dir/lib/postgres-test-databases.sh"

test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
query_log="$test_dir/query.log"
drop_log="$test_dir/drop.log"

timeout() {
  [[ $1 == --kill-after=5s ]]
  shift 2
  "$@"
}

psql() {
  if [[ $* == *'SELECT datname FROM pg_database'* ]]; then
    printf '%s\n' "$*" >"$query_log"
    [[ ${fake_list_status:-0} == 0 ]] || return "$fake_list_status"
    printf '%s' "${fake_databases:-}"
    return
  fi
  printf '%s\n' "$*" >>"$drop_log"
}

run_token=$(postgres_test_run_token)
[[ $run_token =~ ^[a-f0-9]{10}$ ]]

prefix="olp_test_${run_token}_"
fake_databases="${prefix}alpha
${prefix}beta_2"
postgres_test_sweep_databases \
  'postgres://example.invalid/postgres' "$prefix" lower-identifier integration >/dev/null
grep -Fq "WHERE datname ~ '^${prefix}[a-z0-9_]+$'" "$query_log"
grep -Fq "DROP DATABASE IF EXISTS \"${prefix}alpha\" WITH (FORCE)" "$drop_log"
grep -Fq "DROP DATABASE IF EXISTS \"${prefix}beta_2\" WITH (FORCE)" "$drop_log"

: >"$drop_log"
fake_databases="${prefix}safe;drop_database"
if postgres_test_sweep_databases \
  'postgres://example.invalid/postgres' "$prefix" lower-identifier integration \
  >/dev/null 2>&1; then
  echo "unsafe database name was accepted" >&2
  exit 1
fi
[[ ! -s $drop_log ]]

if postgres_test_sweep_databases \
  'postgres://example.invalid/postgres' 'unsafe-prefix_' lower-identifier integration \
  >/dev/null 2>&1; then
  echo "unsafe database prefix was accepted" >&2
  exit 1
fi

runner_log="$test_dir/runner.log"
coverage_runner() {
  printf '%s\n' "$@" > "$runner_log"
}
export -f coverage_runner psql timeout
export drop_log query_log runner_log

valkey_skip_args=(
  --skip distributed_limits_valkey
  --skip distributed_cost_limits_valkey
  --skip request_metadata_consumer_valkey
  --skip spend_controls_postgres::status_and_reconciliation_include_raw_and_exact_hourly_attempts
  --skip spend_controls_postgres::reconciliation_repairs_malformed_state_and_continues_to_later_keys
  --skip spend_recovery_postgres::future_skew_is_excluded_from_today_but_retained_in_its_own_window
)

check_runner_arguments() (
  local valkey_mode=$1
  shift
  export OLP_TEST_DATABASE_ADMIN_URL='postgres://example.invalid/postgres'
  export OLP_TEST_DATABASE_URL_PREFIX='postgres://example.invalid/'
  export OLP_DB_TEST_RUNNER='coverage_runner --no-report'
  case "$valkey_mode" in
    unset) unset OLP_VALKEY_URL ;;
    empty) export OLP_VALKEY_URL='' ;;
    set) export OLP_VALKEY_URL='redis://example.invalid' ;;
  esac
  "$script_dir/run-postgres-tests.sh" "$@"
  local expected_args=(
    --no-report --locked --all-features --package olp-db --package olp
    --profile db --run-ignored ignored-only "$@"
  )
  if [[ $valkey_mode != set ]]; then
    local has_separator=false argument
    for argument in "$@"; do
      if [[ $argument == -- ]]; then has_separator=true; fi
    done
    if [[ $has_separator == false ]]; then expected_args+=(--); fi
    expected_args+=("${valkey_skip_args[@]}")
  fi
  diff -u <(printf '%s\n' "${expected_args[@]}") "$runner_log"
)

for valkey_mode in unset empty set; do
  check_runner_arguments "$valkey_mode"
  check_runner_arguments "$valkey_mode" -E 'test(spend_controls_postgres)' spend
  check_runner_arguments "$valkey_mode" -E 'test(runner_override)' -- --skip caller_skip
  check_runner_arguments "$valkey_mode" -- spend_recovery_postgres
done

postgres_only_cases=(
  spend_controls_postgres::future_window_delta_cannot_replace_the_current_durable_window
  spend_controls_postgres::migration_fences_n_minus_one_rollup_before_raw_fact_deletion
  spend_recovery_postgres::leadership_is_retained_across_follower_ticks_and_released_on_drop
  spend_recovery_postgres::cancelling_the_owner_drops_the_detached_lock_session
)
for test_case in "${postgres_only_cases[@]}"; do
  for ((index = 1; index < ${#valkey_skip_args[@]}; index += 2)); do
    [[ $test_case != *"${valkey_skip_args[index]}"* ]]
  done
done

fake_list_status=7
if postgres_test_sweep_databases \
  'postgres://example.invalid/postgres' "$prefix" lower-identifier integration \
  >/dev/null 2>&1; then
  echo "database listing failure was ignored" >&2
  exit 1
fi

echo "PostgreSQL test database helper contract tests passed"
