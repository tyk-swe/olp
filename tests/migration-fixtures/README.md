# Migration compatibility sidecars

Every SQL migration after `0027` must have a sidecar named after the migration,
for example `0028_scope_expand.contract.json` for
`crates/storage/migrations/0028_scope_expand.sql`.

```json
{
  "schema_version": 1,
  "contract_id": "olp.migration.0028",
  "migration_file": "0028_scope_expand.sql",
  "migration_version": 28,
  "phase": "expand",
  "feature_gate": "enterprise_scope_dual_write",
  "n_minus_one": {
    "gateway": "read_write",
    "control": "read_write",
    "worker": "read_write"
  },
  "rollback_decision": "binary_rollback_safe",
  "contract_of": [],
  "verification": [
    "crates/storage/tests/scope_upgrade_postgres.rs"
  ],
  "unsafe_sql_exceptions": []
}
```

`expand` and `migrate` preserve every N-1 default-scope read and write.
`contract` sidecars use `unsupported_fail_closed` for all three roles, set
`rollback_decision` to `forward_fix_or_restore`, and list the earlier expansion
migration numbers in `contract_of`. Every reference must be a post-M0 expand or
migrate sidecar with the same feature gate, and at least one must be an expand
phase. Contract removal also records its verified zero-state explicitly:

```json
{
  "contract_preconditions": {
    "feature_enabled_successfully": true,
    "legacy_only_rows": 0,
    "n_minus_one_workloads": 0,
    "legacy_queued_events": 0,
    "legacy_idempotency_replays": 0,
    "verification": [
      "crates/storage/tests/example_contract_postgres.rs"
    ]
  }
}
```

A `migrate` sidecar is rejected unless an earlier `expand` sidecar uses the
same feature gate.

The checker recognizes SQL that commonly blocks or breaks old binaries. An
unavoidable match requires exactly one exception with the rule ID, a concrete
rationale, and the approving Linear issue:

```json
{
  "rule_id": "blocking-index-build",
  "reason": "The table is bounded to one row and the measured lock is below the approved budget.",
  "approval": "XOD-123"
}
```

Run `scripts/check-migration-contract.sh` and
`scripts/check-migration-contract.sh --self-test-rules` before review.
