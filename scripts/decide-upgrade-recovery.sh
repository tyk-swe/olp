#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage: scripts/decide-upgrade-recovery.sh EVIDENCE.json

Return the fail-closed recovery decision for an interrupted rollout. The input
is an immutable operator evidence record; this helper never mutates a database.
USAGE
}

if [[ $# -ne 1 || ${1:-} == --help || ${1:-} == -h ]]; then
  usage
  [[ ${1:-} == --help || ${1:-} == -h ]] && exit 0 || exit 2
fi

command -v jq >/dev/null || {
  echo "upgrade recovery decision requires jq" >&2
  exit 2
}

evidence=$1
[[ -f $evidence ]] || {
  echo "upgrade recovery evidence does not exist: $evidence" >&2
  exit 1
}

jq -e '
  type == "object"
  and .schema_version == 1
  and (.last_successful_migration == null
    or (.last_successful_migration | type == "number" and floor == . and . >= 1))
  and (.phase == "none" or .phase == "expand" or .phase == "migrate" or .phase == "contract")
  and (.feature_gate_state | type == "string" and length > 0)
  and (.new_only_write_observed | type == "boolean")
  and (.n_minus_one_workloads_drained | type == "boolean")
  and (.pre_upgrade_backup_and_key_snapshot_ready | type == "boolean")
  and (.forward_fix_within_rto | type == "boolean")
  and (if .phase == "none"
       then .last_successful_migration == null and .new_only_write_observed == false
       else .last_successful_migration != null
       end)
' "$evidence" >/dev/null || {
  echo "upgrade recovery evidence is incomplete or inconsistent: $evidence" >&2
  exit 1
}

phase=$(jq -r '.phase' "$evidence")
new_only_write=$(jq -r '.new_only_write_observed' "$evidence")
forward_fix_within_rto=$(jq -r '.forward_fix_within_rto' "$evidence")
backup_ready=$(jq -r '.pre_upgrade_backup_and_key_snapshot_ready' "$evidence")

decision=
application_rollback_safe=false
case "$phase" in
  none)
    decision=roll_back_application
    application_rollback_safe=true
    ;;
  expand | migrate)
    if [[ $new_only_write == false ]]; then
      application_rollback_safe=true
      if [[ $forward_fix_within_rto == true ]]; then
        decision=forward_fix
      else
        decision=roll_back_application
      fi
    elif [[ $forward_fix_within_rto == true ]]; then
      decision=forward_fix
    else
      decision=restore_pre_upgrade_backup_to_replacement_cluster
    fi
    ;;
  contract)
    if [[ $forward_fix_within_rto == true ]]; then
      decision=forward_fix
    else
      decision=restore_pre_upgrade_backup_to_replacement_cluster
    fi
    ;;
esac

if [[ $decision == restore_pre_upgrade_backup_to_replacement_cluster && $backup_ready != true ]]; then
  echo "safe recovery requires the pre-upgrade backup and matching key snapshot" >&2
  exit 1
fi

jq -n -c \
  --arg decision "$decision" \
  --argjson application_rollback_safe "$application_rollback_safe" \
  '{
    schema_version: 1,
    decision: $decision,
    application_rollback_safe: $application_rollback_safe,
    restore_in_place: false
  }'
