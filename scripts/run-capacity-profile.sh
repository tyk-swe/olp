#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
usage: scripts/run-capacity-profile.sh PROFILE [OPTIONS]

Render and validate a deterministic capacity-profile plan. M0 plan rendering
does not create data, contact a service, mutate state, or generate load.

Profiles:
  scope-cardinality       CP-01
  configuration-compile  CP-02
  gateway-unary          CP-03
  gateway-streaming      CP-04
  event-backlog          CP-05
  runtime-convergence    CP-06
  disaster-recovery      CP-07
  slo-soak               CP-08

Options:
  --contract PATH        Capacity contract (default: frozen repository path)
  --seed INTEGER         Deterministic data seed
  --runs INTEGER         Independent run count
  --duration DURATION    Duration as a positive integer plus s, m, h, or d
  --gateway-replicas N   Gateway replica count in the rendered plan
  --execute              Unavailable until the selected profile milestone
  -h, --help             Show this help

The output is canonical JSON plan material, not load-test or qualification
evidence. --execute always fails while this runner remains in M0 plan-only mode.
USAGE
}

die() {
  echo "capacity profile plan: $*" >&2
  exit 1
}

workspace_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
default_contract=docs/enterprise/contracts/capacity-envelope.json

for command in awk cat cp jq mktemp rm rmdir sha256sum stat; do
  command -v "$command" >/dev/null || die "required command is unavailable: $command"
done

if [[ $# -eq 0 ]]; then
  usage >&2
  exit 2
fi
if [[ $1 == --help || $1 == -h ]]; then
  usage
  exit 0
fi

profile_name=$1
shift
case "$profile_name" in
  scope-cardinality)
    profile_key=scope_cardinality
    expected_profile_id=CP-01
    ;;
  configuration-compile)
    profile_key=configuration_compile
    expected_profile_id=CP-02
    ;;
  gateway-unary)
    profile_key=gateway_unary
    expected_profile_id=CP-03
    ;;
  gateway-streaming)
    profile_key=gateway_streaming
    expected_profile_id=CP-04
    ;;
  event-backlog)
    profile_key=event_backlog
    expected_profile_id=CP-05
    ;;
  runtime-convergence)
    profile_key=runtime_convergence
    expected_profile_id=CP-06
    ;;
  disaster-recovery)
    profile_key=recovery
    expected_profile_id=CP-07
    ;;
  slo-soak)
    profile_key=slo_soak
    expected_profile_id=CP-08
    ;;
  *)
    die "unknown profile '$profile_name'"
    ;;
esac

contract_arg=$default_contract
seed_arg=
runs_arg=
duration_arg=
gateway_replicas_arg=
contract_seen=false
seed_seen=false
runs_seen=false
duration_seen=false
gateway_replicas_seen=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --contract)
      [[ $contract_seen == false ]] || die "--contract may be specified only once"
      [[ $# -ge 2 ]] || die "--contract requires a path"
      contract_arg=$2
      contract_seen=true
      shift 2
      ;;
    --seed)
      [[ $seed_seen == false ]] || die "--seed may be specified only once"
      [[ $# -ge 2 ]] || die "--seed requires an integer"
      seed_arg=$2
      seed_seen=true
      shift 2
      ;;
    --runs)
      [[ $runs_seen == false ]] || die "--runs may be specified only once"
      [[ $# -ge 2 ]] || die "--runs requires an integer"
      runs_arg=$2
      runs_seen=true
      shift 2
      ;;
    --duration)
      [[ $duration_seen == false ]] || die "--duration may be specified only once"
      [[ $# -ge 2 ]] || die "--duration requires a value"
      duration_arg=$2
      duration_seen=true
      shift 2
      ;;
    --gateway-replicas)
      [[ $gateway_replicas_seen == false ]] || die "--gateway-replicas may be specified only once"
      [[ $# -ge 2 ]] || die "--gateway-replicas requires an integer"
      gateway_replicas_arg=$2
      gateway_replicas_seen=true
      shift 2
      ;;
    --execute | --execute=*)
      die "--execute is unavailable until the selected profile implementation milestone"
      ;;
    --help | -h)
      usage
      exit 0
      ;;
    *)
      die "unknown option '$1'"
      ;;
  esac
done

if [[ $contract_arg == /* ]]; then
  contract_candidate=$contract_arg
else
  contract_candidate=$workspace_root/$contract_arg
fi
[[ -f $contract_candidate ]] || die "contract does not exist: $contract_arg"
contract_directory=$(cd "$(dirname "$contract_candidate")" && pwd -P)
contract_absolute=$contract_directory/$(basename "$contract_candidate")
if [[ $contract_absolute == "$workspace_root/"* ]]; then
  contract_display=${contract_absolute#"$workspace_root/"}
else
  contract_display=$contract_absolute
fi

temporary_root=${TMPDIR:-/tmp}
[[ $temporary_root == /* && -d $temporary_root ]] || {
  die "TMPDIR must name an existing absolute directory"
}
snapshot_directory=$(mktemp -d "$temporary_root/olp-capacity-profile-plan.XXXXXX") || {
  die "could not create the contract snapshot directory"
}
[[ -d $snapshot_directory && $snapshot_directory == "$temporary_root/olp-capacity-profile-plan."* ]] || {
  die "mktemp returned an unexpected contract snapshot directory"
}
contract_snapshot=$snapshot_directory/contract.json
plan_snapshot=$snapshot_directory/plan.json
cleanup() {
  rm -f -- "$contract_snapshot" "$plan_snapshot" 2>/dev/null || true
  rmdir -- "$snapshot_directory" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

source_identity() {
  stat --dereference --format='%d:%i:%s:%y:%z' -- "$contract_absolute"
}

source_digest() {
  sha256sum -- "$contract_absolute" | awk '{print $1}'
}

source_identity_before=$(source_identity) || die "could not inspect contract source: $contract_display"
source_sha256_before=$(source_digest) || die "could not hash contract source: $contract_display"
cp -- "$contract_absolute" "$contract_snapshot" || die "could not snapshot contract: $contract_display"
[[ -f $contract_snapshot ]] || die "contract snapshot was not created"
read -r snapshot_sha256 _ < <(sha256sum -- "$contract_snapshot")
source_identity_frozen=$(source_identity) || die "contract source disappeared while it was snapshotted"
source_sha256_frozen=$(source_digest) || die "contract source disappeared while it was snapshotted"
[[ $source_identity_before == "$source_identity_frozen" \
  && $source_sha256_before == "$snapshot_sha256" \
  && $source_sha256_frozen == "$snapshot_sha256" ]] || {
  die "contract source changed while it was snapshotted: $contract_display"
}

# The regression suite uses SIGSTOP to mutate the source deterministically
# after this immutable snapshot exists. This hook performs no file or network
# I/O and is deliberately unavailable through the command-line interface.
if [[ ${OLP_CAPACITY_PLAN_TEST_STOP_AFTER_SNAPSHOT:-false} == true ]]; then
  kill -STOP "$BASHPID"
elif [[ ${OLP_CAPACITY_PLAN_TEST_STOP_AFTER_SNAPSHOT:-false} != false ]]; then
  die "OLP_CAPACITY_PLAN_TEST_STOP_AFTER_SNAPSHOT must be true or false"
fi

jq --exit-status \
  --arg key "$profile_key" \
  --arg profile_id "$expected_profile_id" '
    .schema_version == 1
    and .contract_id == "olp.enterprise.capacity-envelope.v1"
    and .decision_status == "accepted_target"
    and .qualification_status == "not_qualified"
    and .profile_runner_contract.path == "scripts/run-capacity-profile.sh"
    and .profile_runner_contract.present_at_m0 == true
    and .profile_runner_contract.mode_at_m0 == "deterministic_plan_renderer_only"
    and .profile_runner_contract.execute_available_at_m0 == false
    and .profile_runner_contract.plan_renderer_generates_load == false
    and .profile_runner_contract.plan_renderer_mutates_state == false
    and .profile_runner_contract.plan_output_is_qualification_evidence == false
    and (.profile_runner_contract.plan_input_bounds | type == "object")
    and (.profiles[$key].profile_id == $profile_id)
    and (.profiles[$key].implementation_status | startswith("planned_"))
    and .profiles[$key].qualification_status == "not_qualified"
  ' "$contract_snapshot" >/dev/null || {
    die "contract or selected profile is not a valid M0 plan-only contract"
  }

read -r seed_minimum seed_maximum runs_minimum runs_maximum \
  duration_minimum duration_maximum gateway_minimum gateway_maximum < <(
  jq -er '[
    .profile_runner_contract.plan_input_bounds.seed.minimum,
    .profile_runner_contract.plan_input_bounds.seed.maximum,
    .profile_runner_contract.plan_input_bounds.runs.minimum,
    .profile_runner_contract.plan_input_bounds.runs.maximum,
    .profile_runner_contract.plan_input_bounds.duration_seconds.minimum,
    .profile_runner_contract.plan_input_bounds.duration_seconds.maximum,
    .profile_runner_contract.plan_input_bounds.gateway_replicas.minimum,
    .profile_runner_contract.plan_input_bounds.gateway_replicas.maximum
  ] | @tsv' "$contract_snapshot"
)

normalize_integer() {
  local raw=$1 label=$2 minimum=$3 maximum=$4 normalized
  [[ $raw =~ ^[0-9]+$ ]] || die "$label must be an unsigned base-10 integer"
  normalized=$(jq -nr --arg raw "$raw" '$raw | tonumber')
  jq -en \
    --argjson value "$normalized" \
    --argjson minimum "$minimum" \
    --argjson maximum "$maximum" \
    '$value == ($value | floor) and $value >= $minimum and $value <= $maximum' \
    >/dev/null || die "$label must be between $minimum and $maximum"
  printf '%s\n' "$normalized"
}

parse_duration_seconds() {
  local raw=$1 magnitude unit multiplier seconds
  [[ $raw =~ ^([0-9]+)([smhd])$ ]] || {
    die "--duration must be a positive integer followed by s, m, h, or d"
  }
  magnitude=${BASH_REMATCH[1]}
  unit=${BASH_REMATCH[2]}
  case "$unit" in
    s) multiplier=1 ;;
    m) multiplier=60 ;;
    h) multiplier=3600 ;;
    d) multiplier=86400 ;;
  esac
  magnitude=$(normalize_integer "$magnitude" "--duration magnitude" 1 "$duration_maximum")
  seconds=$(jq -nr --argjson magnitude "$magnitude" --argjson multiplier "$multiplier" \
    '$magnitude * $multiplier')
  normalize_integer "$seconds" "--duration in seconds" "$duration_minimum" "$duration_maximum"
}

read -r normative_seed normative_runs normative_duration normative_gateway_replicas < <(
  jq -er --arg key "$profile_key" '
    .profiles[$key] as $profile
    | [
        .profile_defaults.data_seed,
        .profile_defaults.independent_runs,
        ($profile.duration_seconds // $profile.load.duration_seconds // .profile_defaults.measurement_seconds),
        ($profile.load.gateway_replicas // .reference_topology.kubernetes.gateway_replicas)
      ]
    | @tsv
  ' "$contract_snapshot"
)

normative_seed=$(normalize_integer "$normative_seed" "contract data seed" "$seed_minimum" "$seed_maximum")
normative_runs=$(normalize_integer "$normative_runs" "contract run count" "$runs_minimum" "$runs_maximum")
normative_duration=$(normalize_integer "$normative_duration" "contract duration" "$duration_minimum" "$duration_maximum")
normative_gateway_replicas=$(normalize_integer "$normative_gateway_replicas" \
  "contract gateway replica count" "$gateway_minimum" "$gateway_maximum")

seed=$normative_seed
runs=$normative_runs
duration_seconds=$normative_duration
gateway_replicas=$normative_gateway_replicas
[[ $seed_seen == false ]] || seed=$(normalize_integer "$seed_arg" "--seed" "$seed_minimum" "$seed_maximum")
[[ $runs_seen == false ]] || runs=$(normalize_integer "$runs_arg" "--runs" "$runs_minimum" "$runs_maximum")
[[ $duration_seen == false ]] || duration_seconds=$(parse_duration_seconds "$duration_arg")
[[ $gateway_replicas_seen == false ]] || gateway_replicas=$(normalize_integer \
  "$gateway_replicas_arg" "--gateway-replicas" "$gateway_minimum" "$gateway_maximum")

contract_sha256=$snapshot_sha256

jq --sort-keys \
  --arg profile_key "$profile_key" \
  --arg invocation_name "$profile_name" \
  --arg expected_profile_id "$expected_profile_id" \
  --arg contract_path "$contract_display" \
  --arg contract_sha256 "$contract_sha256" \
  --argjson seed "$seed" \
  --argjson runs "$runs" \
  --argjson duration_seconds "$duration_seconds" \
  --argjson gateway_replicas "$gateway_replicas" \
  --argjson normative_seed "$normative_seed" \
  --argjson normative_runs "$normative_runs" \
  --argjson normative_duration "$normative_duration" \
  --argjson normative_gateway_replicas "$normative_gateway_replicas" '
    . as $contract
    | .profiles[$profile_key] as $profile
    | ([
        if $seed == $normative_seed then empty
        else {parameter: "seed", normative: $normative_seed, effective: $seed} end,
        if $runs == $normative_runs then empty
        else {parameter: "runs", normative: $normative_runs, effective: $runs} end,
        if $duration_seconds == $normative_duration then empty
        else {parameter: "duration_seconds", normative: $normative_duration, effective: $duration_seconds} end,
        if $gateway_replicas == $normative_gateway_replicas then empty
        else {parameter: "gateway_replicas", normative: $normative_gateway_replicas, effective: $gateway_replicas} end
      ]) as $deviations
    | {
        schema_version: 1,
        document_type: "olp.enterprise.capacity-profile-plan.v1",
        mode: "plan_only",
        contract: {
          path: $contract_path,
          sha256: $contract_sha256,
          contract_id: $contract.contract_id,
          decision_status: $contract.decision_status,
          approval_status: $contract.approval_status,
          qualification_status: $contract.qualification_status
        },
        profile: {
          key: $profile_key,
          invocation_name: $invocation_name,
          profile_id: $expected_profile_id,
          definition: $profile
        },
        parameters: {
          normative: {
            seed: $normative_seed,
            runs: $normative_runs,
            duration_seconds: $normative_duration,
            gateway_replicas: $normative_gateway_replicas
          },
          effective: {
            seed: $seed,
            runs: $runs,
            duration_seconds: $duration_seconds,
            gateway_replicas: $gateway_replicas
          },
          deviations: $deviations,
          normative_profile: ($deviations | length == 0)
        },
        execution: {
          available: false,
          requested: false,
          generates_load: false,
          mutates_state: false,
          unavailable_while_profile_status: $profile.implementation_status
        },
        qualification: {
          status: $profile.qualification_status,
          evidence_eligible: false,
          reason: "M0_plan_rendering_is_not_execution_or_qualification_evidence"
        },
        frozen_context: {
          reference_topology: $contract.reference_topology,
          beta_targets: $contract.beta_targets,
          slo_targets: $contract.slo_targets,
          propagation_targets: $contract.propagation_targets,
          recovery_targets: $contract.recovery_targets,
          profile_defaults: $contract.profile_defaults
        }
      }
  ' "$contract_snapshot" >"$plan_snapshot"

assert_source_unchanged() {
  local current_identity current_sha256
  current_identity=$(source_identity) || {
    die "contract source disappeared before output completed: $contract_display"
  }
  current_sha256=$(source_digest) || {
    die "contract source disappeared before output completed: $contract_display"
  }
  [[ $current_identity == "$source_identity_frozen" && $current_sha256 == "$contract_sha256" ]] || {
    die "contract source changed before output completed: $contract_display"
  }
}

# Buffer the complete plan before the source check so a change observed before
# emission cannot produce partial stdout. A post-emission check makes a change
# during output a failing invocation; callers must accept plans only on exit 0.
assert_source_unchanged
cat -- "$plan_snapshot"
assert_source_unchanged
