#!/usr/bin/env bash
set -euo pipefail

workspace_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
runner="$workspace_root/scripts/run-capacity-profile.sh"
contract="$workspace_root/docs/enterprise/contracts/capacity-envelope.json"
test_root=$(mktemp -d)
mutation_pid=
cleanup() {
  if [[ -n $mutation_pid ]] && kill -0 "$mutation_pid" 2>/dev/null; then
    kill -CONT "$mutation_pid" 2>/dev/null || true
    kill -TERM "$mutation_pid" 2>/dev/null || true
    wait "$mutation_pid" 2>/dev/null || true
  fi
  rm -rf -- "$test_root"
}
trap cleanup EXIT

for command in jq sha256sum; do
  command -v "$command" >/dev/null || {
    echo "capacity profile plan tests require $command" >&2
    exit 2
  }
done
[[ -x $runner ]] || {
  echo "capacity profile plan runner is missing or not executable" >&2
  exit 1
}

read -r contract_sha256 _ < <(sha256sum "$contract")

assert_normative_plan() {
  local alias=$1 key=$2 profile_id=$3 expected_command=$4 first second declared_command
  shift 4

  first=$($runner "$alias" --contract "$contract" "$@")
  second=$($runner "$alias" --contract "$contract" "$@")
  [[ $first == "$second" ]] || {
    echo "capacity profile plan is not deterministic: $profile_id" >&2
    exit 1
  }

  jq --exit-status \
    --arg key "$key" \
    --arg alias "$alias" \
    --arg profile_id "$profile_id" \
    --arg contract_sha256 "$contract_sha256" \
    --slurpfile source "$contract" '
      .schema_version == 1
      and .document_type == "olp.enterprise.capacity-profile-plan.v1"
      and .mode == "plan_only"
      and .contract.path == "docs/enterprise/contracts/capacity-envelope.json"
      and .contract.sha256 == $contract_sha256
      and .contract.qualification_status == "not_qualified"
      and .profile.key == $key
      and .profile.invocation_name == $alias
      and .profile.profile_id == $profile_id
      and .profile.definition == $source[0].profiles[$key]
      and .profile.definition.qualification_status == "not_qualified"
      and (.profile.definition.implementation_status | startswith("planned_"))
      and .parameters.normative_profile == true
      and .parameters.deviations == []
      and .execution == {
        available: false,
        requested: false,
        generates_load: false,
        mutates_state: false,
        unavailable_while_profile_status: $source[0].profiles[$key].implementation_status
      }
      and .qualification.evidence_eligible == false
      and .qualification.status == "not_qualified"
      and .frozen_context.reference_topology == $source[0].reference_topology
      and .frozen_context.beta_targets == $source[0].beta_targets
      and .frozen_context.slo_targets == $source[0].slo_targets
      and .frozen_context.propagation_targets == $source[0].propagation_targets
      and .frozen_context.recovery_targets == $source[0].recovery_targets
      and .frozen_context.profile_defaults == $source[0].profile_defaults
    ' <<<"$first" >/dev/null || {
      echo "capacity profile plan is incomplete or non-normative: $profile_id" >&2
      exit 1
    }

  declared_command=$(jq -er --arg key "$key" '.profiles[$key].command' "$contract")
  [[ $declared_command == "$expected_command" ]] || {
    echo "capacity profile command drifted for $profile_id" >&2
    exit 1
  }
}

assert_normative_plan \
  scope-cardinality scope_cardinality CP-01 \
  'scripts/run-capacity-profile.sh scope-cardinality --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719' \
  --seed 20260719
assert_normative_plan \
  configuration-compile configuration_compile CP-02 \
  'scripts/run-capacity-profile.sh configuration-compile --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719' \
  --seed 20260719
assert_normative_plan \
  gateway-unary gateway_unary CP-03 \
  'scripts/run-capacity-profile.sh gateway-unary --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719 --runs 3' \
  --seed 20260719 --runs 3
assert_normative_plan \
  gateway-streaming gateway_streaming CP-04 \
  'scripts/run-capacity-profile.sh gateway-streaming --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719 --runs 3' \
  --seed 20260719 --runs 3
assert_normative_plan \
  event-backlog event_backlog CP-05 \
  'scripts/run-capacity-profile.sh event-backlog --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719' \
  --seed 20260719
assert_normative_plan \
  runtime-convergence runtime_convergence CP-06 \
  'scripts/run-capacity-profile.sh runtime-convergence --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719 --gateway-replicas 3' \
  --seed 20260719 --gateway-replicas 3
assert_normative_plan \
  disaster-recovery recovery CP-07 \
  'scripts/run-capacity-profile.sh disaster-recovery --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719' \
  --seed 20260719
assert_normative_plan \
  slo-soak slo_soak CP-08 \
  'scripts/run-capacity-profile.sh slo-soak --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719 --duration 24h' \
  --seed 20260719 --duration 24h

override_plan=$($runner gateway-unary \
  --contract "$contract" \
  --seed 42 \
  --runs 2 \
  --duration 10m \
  --gateway-replicas 4)
jq --exit-status '
  .parameters.normative_profile == false
  and .parameters.effective == {
    seed: 42,
    runs: 2,
    duration_seconds: 600,
    gateway_replicas: 4
  }
  and (.parameters.deviations | map(.parameter)) == [
    "seed", "runs", "duration_seconds", "gateway_replicas"
  ]
  and .execution.generates_load == false
  and .execution.mutates_state == false
  and .qualification.evidence_eligible == false
' <<<"$override_plan" >/dev/null || {
  echo "capacity profile overrides were not rendered deterministically" >&2
  exit 1
}

assert_rejected() {
  local name=$1
  shift
  if "$runner" "$@" >"$test_root/$name.stdout" 2>"$test_root/$name.stderr"; then
    echo "capacity profile runner accepted invalid case: $name" >&2
    exit 1
  fi
  [[ ! -s $test_root/$name.stdout ]] || {
    echo "capacity profile runner emitted a plan for invalid case: $name" >&2
    exit 1
  }
}

assert_rejected unknown-profile unknown-profile --contract "$contract"
assert_rejected invalid-seed gateway-unary --contract "$contract" --seed not-a-number
assert_rejected zero-runs gateway-unary --contract "$contract" --runs 0
assert_rejected unitless-duration slo-soak --contract "$contract" --duration 24
assert_rejected zero-gateways runtime-convergence --contract "$contract" --gateway-replicas 0
assert_rejected execute-unavailable gateway-unary --contract "$contract" --execute

grep -q -- '--execute is unavailable until the selected profile implementation milestone' \
  "$test_root/execute-unavailable.stderr" || {
  echo "capacity profile runner did not explain the M0 execution fence" >&2
  exit 1
}

mutable_contract="$test_root/mutable-capacity-envelope.json"
cp "$contract" "$mutable_contract"
mutable_sha256=$(sha256sum "$mutable_contract" | awk '{print $1}')
stable_snapshot_plan=$($runner gateway-unary --contract "$mutable_contract" --seed 20260719 --runs 3)
jq --exit-status --arg sha256 "$mutable_sha256" --slurpfile source "$mutable_contract" '
  .contract.sha256 == $sha256
  and .profile.definition == $source[0].profiles.gateway_unary
  and .frozen_context.reference_topology == $source[0].reference_topology
  and .frozen_context.beta_targets == $source[0].beta_targets
' <<<"$stable_snapshot_plan" >/dev/null || {
  echo "capacity plan does not bind all reads to the snapshotted contract digest" >&2
  exit 1
}

mutation_stdout="$test_root/concurrent-mutation.stdout"
mutation_stderr="$test_root/concurrent-mutation.stderr"
OLP_CAPACITY_PLAN_TEST_STOP_AFTER_SNAPSHOT=true \
  "$runner" gateway-unary --contract "$mutable_contract" --seed 20260719 --runs 3 \
  >"$mutation_stdout" 2>"$mutation_stderr" &
mutation_pid=$!
runner_stopped=false
for _ in {1..500}; do
  if [[ ! -r /proc/$mutation_pid/status ]]; then
    break
  fi
  runner_state=$(awk '$1 == "State:" {print $2}' "/proc/$mutation_pid/status")
  if [[ $runner_state == T ]]; then
    runner_stopped=true
    break
  fi
  sleep 0.01
done
[[ $runner_stopped == true ]] || {
  echo "capacity profile mutation test could not stop the runner after its snapshot" >&2
  exit 1
}

jq '.interpretation = "concurrent mutation after validated snapshot"' \
  "$mutable_contract" >"$test_root/mutated-contract.json"
mv "$test_root/mutated-contract.json" "$mutable_contract"
kill -CONT "$mutation_pid"
set +e
wait "$mutation_pid"
mutation_status=$?
set -e
mutation_pid=
[[ $mutation_status -ne 0 ]] || {
  echo "capacity profile runner accepted a concurrently changed contract source" >&2
  exit 1
}
[[ ! -s $mutation_stdout ]] || {
  echo "capacity profile runner emitted a plan after a pre-output contract change" >&2
  exit 1
}
grep -q 'contract source changed before output completed' "$mutation_stderr" || {
  echo "capacity profile runner did not report the concurrent contract change" >&2
  exit 1
}

echo "capacity profile plan renderer tests passed"
