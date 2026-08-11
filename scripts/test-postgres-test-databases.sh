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

fake_list_status=7
if postgres_test_sweep_databases \
  'postgres://example.invalid/postgres' "$prefix" lower-identifier integration \
  >/dev/null 2>&1; then
  echo "database listing failure was ignored" >&2
  exit 1
fi

echo "PostgreSQL test database helper contract tests passed"
