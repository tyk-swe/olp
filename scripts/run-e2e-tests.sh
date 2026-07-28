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
#   OLP_E2E_VALKEY_URL          Valkey URL (default redis://localhost:6379)
#   OLP_E2E_BIN                 Prebuilt olp binary; built here when unset
#   OLP_E2E_KEEP_DB=1           Keep the per-run database for debugging
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/cargo-target-dir.sh
source "$repo_root/scripts/lib/cargo-target-dir.sh"

if ! command -v cargo >/dev/null 2>&1; then
  echo "cargo is required" >&2
  exit 1
fi

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
export OLP_E2E_BIN

# One server is booted for the whole binary and shared, so the tests must not
# run concurrently with each other. This is the one suite in the repository
# that uses cargo test rather than nextest: nextest runs every test in its own
# process, which would boot a server per assertion.
cd -- "$repo_root"
exec env SQLX_OFFLINE=true cargo test --locked -p olp-e2e --test contract -- \
  --ignored --test-threads=1
