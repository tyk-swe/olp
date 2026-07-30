#!/usr/bin/env bash
# Runs the end-to-end contract suite (tests/e2e): the production `olp` binary
# against a real PostgreSQL, a real Valkey, and a loopback mock upstream that
# speaks real vendor wire formats.
#
# Assertions here encode the DOCUMENTED contract — README.md, docs/*.md, and
# openapi/management.json — not the current behaviour of the code. A failure is
# a product bug until proven otherwise, and this script exits non-zero on any
# failure: there is no expected-failure manifest and no drift gate.
#
# Never weaken an assertion to make this pass. If an assertion is wrong because
# the documentation is wrong, change the documentation in the same commit and
# say which clause moved.
#
# Environment:
#   OLP_E2E_DATABASE_ADMIN_URL  PostgreSQL maintenance database
#                               (default postgres://olp_test:olp_test@localhost:5433/postgres)
#   OLP_E2E_VALKEY_URL          Dedicated Valkey URL; when unset, the harness
#                               leases and clears an exclusive local logical DB
#   OLP_E2E_TEST_TARGET         contract (default) or ha
#   OLP_E2E_BIN                 Prebuilt olp binary; built here when unset
#   OLP_E2E_KEEP_DB=1           Keep the per-run database for debugging
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/cargo-target-dir.sh
# The dynamic repository root cannot be followed by shellcheck without `-x`.
# shellcheck disable=SC1091
source "$repo_root/scripts/lib/cargo-target-dir.sh"

: "${OLP_E2E_DATABASE_ADMIN_URL:=postgres://olp_test:olp_test@localhost:5433/postgres}"
export OLP_E2E_DATABASE_ADMIN_URL

for command in cargo psql sha256sum timeout; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

run_token=$(printf '%s' "${GITHUB_RUN_ID:-}_${GITHUB_RUN_ATTEMPT:-}_$$_${RANDOM}_$(date +%s%N)" | sha256sum)
run_token=${run_token:0:10}
export OLP_E2E_RUN_TOKEN="$run_token"

sweep_leftover_databases() {
  local leftovers database
  if ! leftovers=$(timeout --kill-after=5s 30s \
    psql "$OLP_E2E_DATABASE_ADMIN_URL" --no-psqlrc --tuples-only --no-align \
    --command "SELECT datname FROM pg_database
               WHERE datname ~ '^olp_e2e_${run_token}_[a-f0-9]+$'"); then
    echo "failed to list E2E databases" >&2
    return 1
  fi
  while IFS= read -r database; do
    [[ -n $database ]] || continue
    if [[ ! $database =~ ^olp_e2e_${run_token}_[a-f0-9]+$ ]]; then
      echo "refusing to drop suspicious E2E database name: $database" >&2
      return 1
    fi
    echo "dropping leftover E2E database $database"
    timeout --kill-after=5s 30s \
      psql "$OLP_E2E_DATABASE_ADMIN_URL" --no-psqlrc --quiet \
      --command "DROP DATABASE IF EXISTS \"$database\" WITH (FORCE)" >/dev/null || return 1
  done <<<"$leftovers"
}

stop_leftover_processes() {
  local directory pid_file pid running_executable expected_executable
  [[ -n ${OLP_E2E_BIN:-} ]] || return 0
  shopt -s nullglob
  expected_executable=$(readlink -f -- "$OLP_E2E_BIN")
  for directory in "${TMPDIR:-/tmp}"/olp-e2e-"$run_token"-*; do
    [[ -d $directory ]] || continue
    for pid_file in "$directory"/*.pid; do
      IFS= read -r pid <"$pid_file" || true
      if [[ $pid =~ ^[1-9][0-9]*$ ]] && kill -0 "$pid" 2>/dev/null; then
        running_executable=$(readlink -f -- "/proc/$pid/exe" 2>/dev/null || true)
        if [[ $running_executable == "$expected_executable" ]]; then
          kill -TERM "$pid" 2>/dev/null || true
          for _ in $(seq 1 100); do
            kill -0 "$pid" 2>/dev/null || break
            sleep 0.1
          done
          kill -KILL "$pid" 2>/dev/null || true
        else
          echo "skipping stale pid $pid: executable no longer matches OLP_E2E_BIN" >&2
        fi
      fi
    done
    if [[ ${OLP_E2E_KEEP_DB:-0} != 1 ]]; then
      rm -rf -- "$directory"
    fi
  done
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if ! stop_leftover_processes; then
    echo "failed to stop an E2E process" >&2
    ((status == 0)) && status=1
  fi
  if [[ ${OLP_E2E_KEEP_DB:-0} != 1 ]] && ! sweep_leftover_databases; then
    echo "failed to sweep leftover E2E databases" >&2
    ((status == 0)) && status=1
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# The suite needs the test-util feature: loopback provider endpoints are
# refused by the egress policy unless the test-only gate is compiled in.
if [[ -z ${OLP_E2E_BIN:-} ]]; then
  (cd -- "$repo_root" && SQLX_OFFLINE=true cargo build --locked -p olp --features test-util)
  OLP_E2E_BIN="$(cargo_target_dir "$repo_root")/debug/olp"
fi
if [[ ! -x ${OLP_E2E_BIN} ]]; then
  echo "OLP_E2E_BIN is not an executable file: ${OLP_E2E_BIN}" >&2
  exit 1
fi
OLP_E2E_BIN=$(readlink -f -- "$OLP_E2E_BIN")
export OLP_E2E_BIN
sweep_leftover_databases

# One server is booted for the whole binary and shared, so the tests must not
# run concurrently with each other. This is the one suite in the repository
# that uses cargo test rather than nextest: nextest runs every test in its own
# process, which would boot a server per assertion.
cd -- "$repo_root"
test_target=${OLP_E2E_TEST_TARGET:-contract}
[[ $test_target == contract || $test_target == ha ]] || {
  echo "OLP_E2E_TEST_TARGET must be contract or ha" >&2
  exit 1
}
env SQLX_OFFLINE=true cargo test --locked -p olp-e2e --test "$test_target" -- \
  --ignored --test-threads=1 "$@"
