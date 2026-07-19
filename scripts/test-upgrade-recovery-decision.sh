#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
helper="$root/scripts/decide-upgrade-recovery.sh"
test_root=$(mktemp -d)
trap 'rm -rf -- "$test_root"' EXIT

write_evidence() {
  local output=$1 phase=$2 new_write=$3 forward_fix=$4 backup=$5
  local migration=28
  [[ $phase != none ]] || migration=null
  jq -n \
    --arg phase "$phase" \
    --argjson migration "$migration" \
    --argjson new_write "$new_write" \
    --argjson forward_fix "$forward_fix" \
    --argjson backup "$backup" \
    '{
      schema_version: 1,
      last_successful_migration: $migration,
      phase: $phase,
      feature_gate_state: "off",
      new_only_write_observed: $new_write,
      n_minus_one_workloads_drained: false,
      pre_upgrade_backup_and_key_snapshot_ready: $backup,
      forward_fix_within_rto: $forward_fix
    }' >"$output"
}

assert_decision() {
  local name=$1 phase=$2 new_write=$3 forward_fix=$4 backup=$5 expected=$6
  local evidence="$test_root/$name.json" result
  write_evidence "$evidence" "$phase" "$new_write" "$forward_fix" "$backup"
  result=$($helper "$evidence")
  jq -e --arg expected "$expected" '
    .schema_version == 1
    and .decision == $expected
    and .restore_in_place == false
  ' <<<"$result" >/dev/null || {
    echo "unexpected recovery decision for $name: $result" >&2
    exit 1
  }
}

assert_decision before_database_change none false false true roll_back_application
assert_decision expand_forward_fix expand false true true forward_fix
assert_decision expand_binary_rollback expand false false true roll_back_application
assert_decision migrate_after_new_write migrate true true true forward_fix
assert_decision migrate_restore migrate true false true restore_pre_upgrade_backup_to_replacement_cluster
assert_decision contract_restore contract false false true restore_pre_upgrade_backup_to_replacement_cluster

missing_backup="$test_root/missing-backup.json"
write_evidence "$missing_backup" contract false false false
if "$helper" "$missing_backup" >/dev/null 2>&1; then
  echo "restore decision unexpectedly accepted missing backup/key evidence" >&2
  exit 1
fi

echo "upgrade recovery decision tests passed"
