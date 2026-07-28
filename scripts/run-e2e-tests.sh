#!/usr/bin/env bash
set -euo pipefail

# Runs the true end-to-end journey suite (tests/e2e): the production `olp`
# binary against real PostgreSQL, real Valkey, and a loopback mock upstream
# provider. The suite's assertions encode the DOCUMENTED contract, so tests
# that expose real product bugs stay honestly red; this script gates on
# drift from the committed manifest instead of raw test success:
#
#   - a failure listed in tests/e2e/known-failures.txt is a documented
#     product bug and does not fail the run;
#   - a failure that is not listed fails the run (new regression);
#   - a listed test that now passes fails the run (fix landed - prune the
#     manifest entry in the same change).
#
# Never weaken an assertion to make it pass. Environment:
#   OLP_E2E_DATABASE_ADMIN_URL  PostgreSQL maintenance database
#                               (default postgres://olp_test:olp_test@localhost:5433/postgres)
#   OLP_E2E_VALKEY_URL          Valkey URL (default redis://localhost:6379)
#   OLP_E2E_BIN                 prebuilt binary; built here when unset
#   OLP_E2E_KEEP_DB=1           keep the per-run database for debugging

repo_root="$(cd "$(dirname "$0")/.." && pwd -P)"
# shellcheck source=scripts/lib/cargo-target-dir.sh
source "$repo_root/scripts/lib/cargo-target-dir.sh"
cd "$repo_root"

for required_command in cargo awk sort comm mktemp tee; do
  command -v "$required_command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $required_command" >&2
    exit 1
  }
done

manifest="$repo_root/tests/e2e/known-failures.txt"
[[ -f $manifest ]] || {
  echo "known-failures manifest is missing: $manifest" >&2
  exit 1
}

# The suite drives the exact binary a deployment runs, in the debug profile
# with the test-util feature so the loopback-endpoint opt-in exists.
if [[ -z ${OLP_E2E_BIN:-} ]]; then
  SQLX_OFFLINE=true cargo build --locked -p olp --features test-util
  OLP_E2E_BIN="$(cargo_target_dir "$repo_root")/debug/olp"
fi
[[ -x $OLP_E2E_BIN ]] || {
  echo "olp binary is missing or not executable: $OLP_E2E_BIN" >&2
  exit 1
}
export OLP_E2E_BIN

test_output=$(mktemp)
cleanup() {
  rm -f "$test_output"
}
trap cleanup EXIT

# The journey runs once; assertion tests share its recorded report, so they
# must stay in one process (--test-threads=1 keeps libtest output stable).
test_status=0
SQLX_OFFLINE=true cargo test --locked -p olp-e2e --test journey -- \
  --ignored --test-threads=1 2>&1 | tee "$test_output" || test_status=$?

# A run that never reached libtest's summary (compile error, harness abort)
# has no drift to evaluate and must fail as-is.
if ! awk '/^test result:/ { found = 1 } END { exit !found }' "$test_output"; then
  echo "e2e suite did not produce a libtest summary (exit $test_status)" >&2
  exit $((test_status == 0 ? 1 : test_status))
fi

actual_failures=$(awk '/^test [^ ]+ \.\.\. FAILED$/ { print $2 }' "$test_output" | sort -u)
expected_failures=$(awk '!/^[[:space:]]*(#|$)/ { print $1 }' "$manifest" | sort -u)

drift=0
while IFS= read -r test_name; do
  [[ -n $test_name ]] || continue
  echo "NEW FAILURE (not in known-failures.txt): $test_name" >&2
  drift=1
done < <(comm -13 <(printf '%s\n' "$expected_failures") <(printf '%s\n' "$actual_failures"))

while IFS= read -r test_name; do
  [[ -n $test_name ]] || continue
  echo "FIXED (prune from known-failures.txt): $test_name" >&2
  drift=1
done < <(comm -23 <(printf '%s\n' "$expected_failures") <(printf '%s\n' "$actual_failures"))

if ((drift)); then
  exit 1
fi

known_count=$(printf '%s' "$expected_failures" | awk 'NF { count++ } END { print count + 0 }')
echo "e2e drift gate: failure set matches known-failures.txt (${known_count} documented bugs)"
