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
  git_sha: "0123456789abcdef", valid: true, invalid_reasons: [],
  admission_rejections: 0, machine: {cpu: "fixture", oha: "1.12.0"},
  scenarios: [
    {name: "added", added_latency_ms: {p95: 10, p99: 20},
     gateway: {latency_ms: {p95: 210, p99: 220}, throughput_rps: 100}},
    {name: "gateway",
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
tap_plan
