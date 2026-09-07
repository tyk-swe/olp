#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

# shellcheck source=scripts/lib/tap.sh
source "$script_dir/lib/tap.sh"

baseline="$test_root/baseline.json"
current="$test_root/current.json"
report="$test_root/report.md"
warnings="$test_root/warnings.txt"
jq -n '{
  schema_version: 2, git_sha: "0123456789abcdef", source_dirty: false,
  source_fingerprint: "baseline-fingerprint", valid: true, invalid_reasons: [],
  duration_seconds: 60, build_profile: "release", admission_rejections: 0,
  machine: {cpu: "fixture", platform: "Linux-fixture", logical_cpus: 8,
            rustc: "rustc-fixture", oha: "1.12.0"},
  mock: {unary_delay_ms: 200, stream_tokens: 50},
  scenarios: [
    {name: "added", duration_seconds: 60, concurrency: 16,
     mock: {latency_ms: {p95: 200, p99: 200}, throughput_rps: 100},
     added_latency_ms: {p95: 10, p99: 20},
     gateway: {latency_ms: {p95: 210, p99: 220}, throughput_rps: 100}},
    {name: "gateway", duration_seconds: 60, concurrency: 256,
     gateway: {latency_ms: {p95: 5, p99: 8}, throughput_rps: 100}}
  ]
}' > "$baseline"
jq '.scenarios[].gateway.throughput_rps = 70' "$baseline" > "$current"

render_report() {
  GITHUB_STEP_SUMMARY='' python3 "$script_dir/bench-report.py" "$1" \
    --baseline "$2" --output "$report" > "$warnings"
}

throughput_loss_is_reported() {
  local basis=$1 latency=$2 tail=$3
  render_report "$current" "$baseline" || return
  grep -Fq "| $basis | $basis | $latency ms | $tail ms | $latency ms | p95 +0.0%, throughput -30.0% |" "$report" &&
    grep -Fq -- "- $basis: gateway throughput decreased 30.0%" "$report" &&
    grep -Fxq "::warning::$basis: gateway throughput decreased 30.0%" "$warnings"
}

run_test "positive added latency includes throughput loss and warning" \
  throughput_loss_is_reported added 10.00 20.00
run_test "gateway latency includes throughput loss and warning" \
  throughput_loss_is_reported gateway 5.00 8.00

throughput_change_without_warning() {
  local throughput=$1 change=$2
  jq --argjson throughput "$throughput" \
    '.scenarios[].gateway.throughput_rps = $throughput' "$current" > "$test_root/change.json" || return
  render_report "$test_root/change.json" "$baseline" || return
  [[ $(grep -Fc "throughput $change%" "$report") == 2 ]] &&
    grep -Fq 'No performance regression exceeded 25%.' "$report" &&
    [[ ! -s "$warnings" ]]
}

run_test "exactly 25 percent loss does not warn for either latency basis" \
  throughput_change_without_warning 75 -25.0
run_test "throughput improvements appear for both latency bases" \
  throughput_change_without_warning 110 +10.0

invalid_runs_are_excluded() {
  local side=$1 filter current_path baseline_path expected_warning
  for filter in '.valid = false' 'del(.valid)' \
    '.invalid_reasons = ["fixture invalid"]' '.admission_rejections = 1'; do
    current_path=$current
    baseline_path=$baseline
    expected_warning='::warning::invalid benchmark'
    if [[ $side == current ]]; then
      jq "$filter" "$current" > "$test_root/invalid.json" || return
      current_path="$test_root/invalid.json"
    else
      jq "$filter" "$baseline" > "$test_root/invalid.json" || return
      baseline_path="$test_root/invalid.json"
      expected_warning='::warning::invalid previous benchmark was ignored'
    fi
    render_report "$current_path" "$baseline_path" || return
    grep -Fq "$expected_warning" "$warnings" || return
    ! grep -Eq 'p95 [+-]|throughput [+-]' "$report" || return
  done
}

run_test "invalid current results exclude all comparisons" invalid_runs_are_excluded current
run_test "invalid baselines exclude all comparisons" invalid_runs_are_excluded baseline

zero_baseline_throughput_is_excluded() {
  jq '.scenarios[].gateway.throughput_rps = 0' "$baseline" > "$test_root/zero.json" || return
  render_report "$current" "$test_root/zero.json" || return
  [[ $(grep -Fc 'p95 +0.0%' "$report") == 2 ]] &&
    ! grep -Fq 'throughput' "$report" && [[ ! -s "$warnings" ]]
}

run_test "zero prior throughput excludes only throughput comparison" zero_baseline_throughput_is_excluded

missing_baseline_is_excluded() {
  render_report "$current" "$test_root/missing.json" || return
  ! grep -Eq 'p95 [+-]|throughput [+-]' "$report" &&
    grep -Fq 'No previous main-branch benchmark artifact was available.' "$report" &&
    [[ ! -s "$warnings" ]]
}

run_test "missing baseline leaves comparison unavailable" missing_baseline_is_excluded
source_identity_changes_remain_comparable() {
  jq '.git_sha = "fedcba9876543210" | .source_dirty = true |
      .source_fingerprint = "changed-fingerprint" | .schema_version = 1' \
    "$baseline" > "$test_root/revision.json" || return
  render_report "$test_root/revision.json" "$baseline" || return
  [[ $(grep -Fc 'p95 +0.0%, throughput +0.0%' "$report") == 2 ]] &&
    grep -Fq 'fedcba987654-dirty-changed-fing' "$report" && [[ ! -s "$warnings" ]]
}
run_test "different source revisions and fingerprints remain comparable" source_identity_changes_remain_comparable

all_comparisons_are_unavailable() {
  render_report "$1" "$2" || return
  grep -Fq "$3" "$warnings" &&
    grep -Fq '| added | added | 10.00 ms | 20.00 ms | — | — |' "$report" &&
    grep -Fq '| gateway | gateway | 5.00 ms | 8.00 ms | — | — |' "$report" &&
    ! grep -Eq 'p95 [+-]|throughput [+-]|No performance regression' "$report"
}

global_metadata_change_is_excluded() {
  local field=$1 value=$2
  jq "$field = $value" "$current" > "$test_root/incompatible.json" || return
  all_comparisons_are_unavailable "$test_root/incompatible.json" "$baseline" \
    "comparison unavailable: ${field#.} differs"
}
for field in cpu platform rustc oha; do
  run_test "changed machine $field excludes all comparisons" \
    global_metadata_change_is_excluded ".machine.$field" '"different"'
done
run_test "changed logical CPU count excludes all comparisons" \
  global_metadata_change_is_excluded '.machine.logical_cpus' 4
run_test "changed build profile excludes all comparisons" \
  global_metadata_change_is_excluded '.build_profile' '"debug"'
run_test "changed run duration excludes all comparisons" \
  global_metadata_change_is_excluded '.duration_seconds' 20

only_added_comparison_is_unavailable() {
  render_report "$1" "$2" || return
  grep -Fq "added: comparison unavailable: $3" "$warnings" &&
    grep -Fq '| added | added | 10.00 ms | 20.00 ms | — | — |' "$report" &&
    grep -Fq '| gateway | gateway | 5.00 ms | 8.00 ms | 5.00 ms | p95 +0.0%, throughput -30.0% |' "$report" &&
    ! grep -Fq 'added: gateway throughput decreased' "$warnings"
}

scenario_metadata_change_is_excluded() {
  local field=$1
  jq ".scenarios[0].$field *= 2" "$current" > "$test_root/scenario.json" || return
  only_added_comparison_is_unavailable "$test_root/scenario.json" "$baseline" "$field differs"
}
run_test "changed concurrency excludes only the affected scenario" scenario_metadata_change_is_excluded concurrency
run_test "changed scenario duration excludes only the affected scenario" scenario_metadata_change_is_excluded duration_seconds

mock_metadata_change_is_excluded() {
  local field=$1
  jq ".mock.$field *= 2" "$current" > "$test_root/mock.json" || return
  only_added_comparison_is_unavailable "$test_root/mock.json" "$baseline" "mock.$field differs"
}
run_test "changed unary delay excludes upstream comparisons" mock_metadata_change_is_excluded unary_delay_ms
run_test "changed stream token count excludes upstream comparisons" mock_metadata_change_is_excluded stream_tokens

missing_run_metadata_is_excluded() {
  local side=$1 field current_path baseline_path
  for field in machine.cpu machine.platform machine.logical_cpus machine.rustc machine.oha \
    build_profile duration_seconds; do
    current_path=$current
    baseline_path=$baseline
    if [[ $side != previous ]]; then
      jq "del(.$field)" "$current" > "$test_root/missing-current.json" || return
      current_path="$test_root/missing-current.json"
    fi
    if [[ $side != current ]]; then
      jq "del(.$field)" "$baseline" > "$test_root/missing-previous.json" || return
      baseline_path="$test_root/missing-previous.json"
    fi
    local expected_side=$side
    if [[ $side == both ]]; then expected_side='current and previous'; fi
    all_comparisons_are_unavailable "$current_path" "$baseline_path" \
      "missing $field metadata in $expected_side" || return
  done
}
for side in current previous both; do
  run_test "missing $side run metadata never implies equivalence" missing_run_metadata_is_excluded "$side"
done

unknown_metadata_is_excluded() {
  local filter
  for filter in '.machine.cpu = "unknown"' '.machine.logical_cpus = null' \
    '.machine.rustc = ""' '.build_profile = "external"'; do
    jq "$filter" "$current" > "$test_root/unknown-current.json" || return
    jq "$filter" "$baseline" > "$test_root/unknown-previous.json" || return
    all_comparisons_are_unavailable "$test_root/unknown-current.json" "$test_root/unknown-previous.json" \
      'metadata in current and previous' || return
  done
  for filter in 'del(.machine)' '.machine = null'; do
    jq "$filter" "$current" > "$test_root/unknown-current.json" || return
    all_comparisons_are_unavailable "$test_root/unknown-current.json" "$baseline" \
      'missing machine.cpu metadata in current' || return
    grep -Fq 'Machine: unknown · oha unknown' "$report" || return
  done
}
run_test "unknown or absent machine and build metadata stays readable" unknown_metadata_is_excluded

missing_workload_metadata_is_excluded() {
  local field side current_path baseline_path
  for field in 'scenarios[0].duration_seconds' 'scenarios[0].concurrency' \
    mock.unary_delay_ms mock.stream_tokens; do
    for side in current previous both; do
      current_path=$current
      baseline_path=$baseline
      if [[ $side != previous ]]; then
        jq "del(.$field)" "$current" > "$test_root/missing-current.json" || return
        current_path="$test_root/missing-current.json"
      fi
      if [[ $side != current ]]; then
        jq "del(.$field)" "$baseline" > "$test_root/missing-previous.json" || return
        baseline_path="$test_root/missing-previous.json"
      fi
      local name=$field expected_side=$side
      if [[ $field == scenarios* ]]; then name=${field#*.}; fi
      if [[ $side == both ]]; then expected_side='current and previous'; fi
      only_added_comparison_is_unavailable "$current_path" "$baseline_path" \
        "missing $name metadata in $expected_side" || return
    done
  done
}
run_test "missing workload metadata excludes only affected scenarios" missing_workload_metadata_is_excluded

direct_mock_phase_changes_are_excluded() {
  jq 'del(.scenarios[0].mock)' "$current" > "$test_root/missing-mock-current.json" || return
  jq 'del(.scenarios[0].mock)' "$baseline" > "$test_root/missing-mock-previous.json" || return
  only_added_comparison_is_unavailable "$test_root/missing-mock-current.json" "$test_root/missing-mock-previous.json" \
    'missing direct-mock phase metadata' || return
  jq '.scenarios[1].mock = {}' "$current" > "$test_root/phase.json" || return
  render_report "$test_root/phase.json" "$baseline" || return
  grep -Fq 'gateway: comparison unavailable: direct-mock phase differs' "$warnings" &&
    grep -Fq '| gateway | gateway | 5.00 ms | 8.00 ms | — | — |' "$report" &&
    grep -Fq '| added | added | 10.00 ms | 20.00 ms | 10.00 ms | p95 +0.0%, throughput -30.0% |' "$report"
}
run_test "missing or different direct-mock phases cannot be compared" direct_mock_phase_changes_are_excluded

historical_capture_is_readable() {
  local historical="$script_dir/../bench/results/aa50f62720231408fa4ba5c5bac6411d83caff2e.json"
  render_report "$historical" "$baseline" || return
  grep -Fq '::warning::invalid benchmark' "$warnings" &&
    grep -Fq '| models_c256 | gateway |' "$report" &&
    grep -Fq '(dirty tree)' "$report" &&
    ! grep -Eq 'p95 [+-]|throughput [+-]' "$report"
}
run_test "historical capture remains readable under existing validity rules" historical_capture_is_readable

tap_plan
