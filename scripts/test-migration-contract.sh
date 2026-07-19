#!/usr/bin/env bash
set -euo pipefail

workspace_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d)
trap 'rm -rf -- "$test_root"' EXIT

mkdir -p \
  "$test_root/scripts" \
  "$test_root/docs/enterprise/contracts" \
  "$test_root/crates/storage/migrations" \
  "$test_root/tests/migration-fixtures" \
  "$test_root/tests/migration-contract-evidence"

cp "$workspace_root/scripts/check-migration-contract.sh" "$test_root/scripts/"
cp "$workspace_root/scripts/normalize-sql-comments.pl" "$test_root/scripts/"
cp "$workspace_root/docs/enterprise/contracts/compatibility.json" "$test_root/docs/enterprise/contracts/"
cp "$workspace_root/tests/migration-fixtures/representative-2x.fixture-manifest.json" "$test_root/tests/migration-fixtures/"
cp "$workspace_root"/crates/storage/migrations/*.sql "$test_root/crates/storage/migrations/"

while IFS= read -r evidence; do
  mkdir -p "$test_root/$(dirname "$evidence")"
  : >"$test_root/$evidence"
done < <(jq -r '.provisional_current_evidence[].path' \
  "$test_root/tests/migration-fixtures/representative-2x.fixture-manifest.json")

: >"$test_root/tests/migration-contract-evidence/expand"
: >"$test_root/tests/migration-contract-evidence/migrate"
: >"$test_root/tests/migration-contract-evidence/contract"

printf '%s\n' 'ALTER TABLE settings ADD COLUMN organization_id uuid;' \
  >"$test_root/crates/storage/migrations/0028_scope_expand.sql"
jq -n '{
  schema_version: 1,
  contract_id: "olp.migration.0028",
  migration_file: "0028_scope_expand.sql",
  migration_version: 28,
  phase: "expand",
  feature_gate: "enterprise_scope",
  n_minus_one: {gateway: "read_write", control: "read_write", worker: "read_write"},
  rollback_decision: "binary_rollback_safe",
  contract_of: [],
  verification: ["tests/migration-contract-evidence/expand"],
  unsafe_sql_exceptions: []
}' >"$test_root/tests/migration-fixtures/0028_scope_expand.contract.json"

printf '%s\n' 'SELECT 1;' \
  >"$test_root/crates/storage/migrations/0029_scope_migrate.sql"
jq -n '{
  schema_version: 1,
  contract_id: "olp.migration.0029",
  migration_file: "0029_scope_migrate.sql",
  migration_version: 29,
  phase: "migrate",
  feature_gate: "enterprise_scope",
  n_minus_one: {gateway: "read_write", control: "read_write", worker: "read_write"},
  rollback_decision: "binary_rollback_safe",
  contract_of: [],
  verification: ["tests/migration-contract-evidence/migrate"],
  unsafe_sql_exceptions: []
}' >"$test_root/tests/migration-fixtures/0029_scope_migrate.contract.json"

printf '%s\n' 'ALTER TABLE settings DROP COLUMN legacy_scope;' \
  >"$test_root/crates/storage/migrations/0030_scope_contract.sql"
jq -n '{
  schema_version: 1,
  contract_id: "olp.migration.0030",
  migration_file: "0030_scope_contract.sql",
  migration_version: 30,
  phase: "contract",
  feature_gate: "enterprise_scope",
  n_minus_one: {
    gateway: "unsupported_fail_closed",
    control: "unsupported_fail_closed",
    worker: "unsupported_fail_closed"
  },
  rollback_decision: "forward_fix_or_restore",
  contract_of: [28, 29],
  contract_preconditions: {
    feature_enabled_successfully: true,
    legacy_only_rows: 0,
    n_minus_one_workloads: 0,
    legacy_queued_events: 0,
    legacy_idempotency_replays: 0,
    verification: ["tests/migration-contract-evidence/contract"]
  },
  verification: ["tests/migration-contract-evidence/contract"],
  unsafe_sql_exceptions: []
}' >"$test_root/tests/migration-fixtures/0030_scope_contract.contract.json"

"$test_root/scripts/check-migration-contract.sh" >/dev/null

contract_sidecar="$test_root/tests/migration-fixtures/0030_scope_contract.contract.json"
jq '.unsafe_sql_exceptions = [{
  rule_id: "drop-object",
  reason: "contract phase already permits this removal",
  approval: "XOD-999"
}]' "$contract_sidecar" >"$contract_sidecar.tmp"
mv "$contract_sidecar.tmp" "$contract_sidecar"
if "$test_root/scripts/check-migration-contract.sh" >/dev/null 2>&1; then
  echo "migration checker accepted an unnecessary unsafe-SQL exception" >&2
  exit 1
fi
jq '.unsafe_sql_exceptions = []' "$contract_sidecar" >"$contract_sidecar.tmp"
mv "$contract_sidecar.tmp" "$contract_sidecar"

jq '.contract_of = [29]' "$contract_sidecar" >"$contract_sidecar.tmp"
mv "$contract_sidecar.tmp" "$contract_sidecar"
if "$test_root/scripts/check-migration-contract.sh" >/dev/null 2>&1; then
  echo "migration checker accepted a contract phase without an expand reference" >&2
  exit 1
fi
jq '.contract_of = [28, 29]' "$contract_sidecar" >"$contract_sidecar.tmp"
mv "$contract_sidecar.tmp" "$contract_sidecar"

printf '%s\n' 'DROP/**/TABLE settings;' \
  >"$test_root/crates/storage/migrations/0031_unsafe_expand.sql"
jq -n '{
  schema_version: 1,
  contract_id: "olp.migration.0031",
  migration_file: "0031_unsafe_expand.sql",
  migration_version: 31,
  phase: "expand",
  feature_gate: "unsafe_test",
  n_minus_one: {gateway: "read_write", control: "read_write", worker: "read_write"},
  rollback_decision: "binary_rollback_safe",
  contract_of: [],
  verification: ["tests/migration-contract-evidence/expand"],
  unsafe_sql_exceptions: []
}' >"$test_root/tests/migration-fixtures/0031_unsafe_expand.contract.json"
if "$test_root/scripts/check-migration-contract.sh" >/dev/null 2>&1; then
  echo "migration checker accepted destructive SQL in an expand phase" >&2
  exit 1
fi

echo "migration contract integration tests passed"
