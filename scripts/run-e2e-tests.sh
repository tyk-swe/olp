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

for command in cargo flock git psql sync timeout; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

# Each checkout owns one persistent namespace and lock. Descendants inherit the
# lock, preventing a new run from sweeping resources while any old process is
# alive.
git_dir=$(git -C "$repo_root" rev-parse --absolute-git-dir)
run_state="$git_dir/olp-e2e-run-state"
exec {run_lock_fd}<>"$run_state"
flock --exclusive --nonblock "$run_lock_fd" || {
  echo "another E2E run already owns this checkout" >&2
  exit 75
}
if [[ ! -s $run_state ]]; then
  IFS= read -r run_token </proc/sys/kernel/random/uuid || true
  run_token=${run_token//-/}
  run_token=${run_token:0:10}
  [[ $run_token =~ ^[a-f0-9]{10}$ ]] || {
    echo "failed to generate an E2E database namespace" >&2
    exit 1
  }
  printf '%s\n' "$run_token" >&"$run_lock_fd"
  sync --file-system "$run_state"
else
  run_token=$(<"$run_state")
fi
[[ $run_token =~ ^[a-f0-9]{10}$ ]] || {
  echo "invalid E2E database namespace: $run_state" >&2
  exit 1
}
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

process_start_time() {
  local stat
  local -a fields
  IFS= read -r stat <"/proc/$1/stat" || return 1
  stat=${stat##*) }
  read -ra fields <<<"$stat"
  [[ ${fields[19]:-} =~ ^[1-9][0-9]*$ ]] || return 1
  printf '%s\n' "${fields[19]}"
}

stop_leftover_processes() {
  local directory pid_file pid recorded_started extra current_started
  local running_executable expected_executable unresolved=0
  [[ -n ${OLP_E2E_BIN:-} ]] || return 0
  shopt -s nullglob
  expected_executable=$(readlink -f -- "$OLP_E2E_BIN")
  for directory in "${TMPDIR:-/tmp}"/olp-e2e-"$run_token"-*; do
    [[ -d $directory ]] || continue
    for pid_file in "$directory"/*.pid; do
      IFS=' ' read -r pid recorded_started extra <"$pid_file" || true
      if [[ ! $pid =~ ^[1-9][0-9]*$ \
        || ! $recorded_started =~ ^[1-9][0-9]*$ || -n $extra ]]; then
        echo "refusing cleanup with malformed process identity: $pid_file" >&2
        unresolved=1
        continue
      fi
      kill -0 "$pid" 2>/dev/null || continue
      current_started=$(process_start_time "$pid" 2>/dev/null || true)
      if [[ $current_started == "$recorded_started" ]]; then
        running_executable=$(readlink -f -- "/proc/$pid/exe" 2>/dev/null || true)
        if [[ $running_executable == "$expected_executable" \
          || $running_executable == "$expected_executable (deleted)" ]]; then
          kill -TERM "$pid" 2>/dev/null || true
          for _ in {1..100}; do
            current_started=$(process_start_time "$pid" 2>/dev/null || true)
            [[ $current_started == "$recorded_started" ]] || break
            sleep 0.1
          done
          current_started=$(process_start_time "$pid" 2>/dev/null || true)
          if [[ $current_started == "$recorded_started" ]]; then
            kill -KILL "$pid" 2>/dev/null || true
          fi
        else
          echo "skipping stale pid $pid: executable no longer matches OLP_E2E_BIN" >&2
          unresolved=1
        fi
      elif [[ -z $current_started ]] && kill -0 "$pid" 2>/dev/null; then
        echo "unable to verify process identity for live pid $pid" >&2
        unresolved=1
      fi
    done
    if (( unresolved == 0 )) && [[ ${OLP_E2E_KEEP_DB:-0} != 1 ]]; then
      rm -rf -- "$directory"
    fi
  done
  (( unresolved == 0 ))
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if ! stop_leftover_processes; then
    echo "failed to stop an E2E process" >&2
    ((status == 0)) && status=1
  elif [[ ${OLP_E2E_KEEP_DB:-0} != 1 ]] && ! sweep_leftover_databases; then
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
stop_leftover_processes
if [[ ${OLP_E2E_KEEP_DB:-0} != 1 ]]; then
  sweep_leftover_databases
fi

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
