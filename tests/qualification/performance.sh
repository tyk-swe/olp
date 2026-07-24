#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: tests/qualification/performance.sh load|soak" >&2
}
if [[ $# -ne 1 || ${1:-} == --help || ${1:-} == -h ]]; then
  usage
  [[ $# -eq 1 ]] && exit 0 || exit 2
fi
profile=$1
[[ $profile == load || $profile == soak ]] || { usage; exit 2; }

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
for command in cargo jq k6; do
  command -v "$command" >/dev/null || { echo "required command is unavailable: $command" >&2; exit 1; }
done

artifact_dir=${OLP_QUALIFICATION_ARTIFACT_DIR:-"$root/artifacts/qualification/$profile"}
mkdir -p "$artifact_dir"
work=$(mktemp -d)
fixture_pid=
sampler_pid=
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  [[ -z $sampler_pid ]] || kill "$sampler_pid" 2>/dev/null || true
  [[ -z $fixture_pid ]] || kill "$fixture_pid" 2>/dev/null || true
  [[ -z $sampler_pid ]] || wait "$sampler_pid" 2>/dev/null || true
  [[ -z $fixture_pid ]] || wait "$fixture_pid" 2>/dev/null || true
  if (( status != 0 )); then
    tail -n 240 "$artifact_dir/server.log" >&2 || true
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cargo build --locked -p olp --example sdk_smoke_fixture
target_dir=${CARGO_TARGET_DIR:-"$root/target"}
[[ $target_dir == /* ]] || target_dir="$root/$target_dir"
OLP_SDK_SMOKE_METADATA="$work/metadata.json" \
  "$target_dir/debug/examples/sdk_smoke_fixture" >"$artifact_dir/server.log" 2>&1 &
fixture_pid=$!
for _ in $(seq 1 300); do
  [[ -s $work/metadata.json ]] && jq -e . "$work/metadata.json" >/dev/null 2>&1 && break
  kill -0 "$fixture_pid" 2>/dev/null || { echo "fixture exited during startup" >&2; exit 1; }
  sleep 0.1
done
origin=$(jq -er .origin "$work/metadata.json")
api_key=$(jq -er .api_key "$work/metadata.json")
spool="$work/spool"
mkdir "$spool"

if [[ $profile == soak ]]; then
  samples="$artifact_dir/resources.tsv"
  printf 'elapsed_seconds\trss_kib\tfds\tthreads\n' > "$samples"
  (
    started=$SECONDS
    while kill -0 "$fixture_pid" 2>/dev/null; do
      rss=$(awk '/^VmRSS:/ {print $2}' "/proc/$fixture_pid/status")
      threads=$(awk '/^Threads:/ {print $2}' "/proc/$fixture_pid/status")
      fds=$(find "/proc/$fixture_pid/fd" -mindepth 1 -maxdepth 1 -printf . 2>/dev/null | wc -c)
      printf '%s\t%s\t%s\t%s\n' "$((SECONDS - started))" "${rss:-0}" "$fds" "${threads:-0}" >> "$samples"
      sleep "${OLP_QUALIFICATION_SAMPLE_INTERVAL_SECONDS:-60}"
    done
  ) &
  sampler_pid=$!
fi

export OLP_QUALIFICATION_ORIGIN="$origin"
export OLP_QUALIFICATION_API_KEY="$api_key"
export OLP_QUALIFICATION_PROFILE="$profile"
export OLP_QUALIFICATION_SUMMARY="$artifact_dir/summary.json"
k6 run --summary-export "$artifact_dir/k6-summary.json" "$root/tests/qualification/load.js" \
  >"$artifact_dir/k6.log" 2>&1

if [[ $profile == soak ]]; then
  kill "$sampler_pid" 2>/dev/null || true
  wait "$sampler_pid" 2>/dev/null || true
  sampler_pid=
  baseline=$(awk -F '\t' 'NR==2 {print $2}' "$samples")
  mapfile -t final < <(awk -F '\t' 'NR>1 && $1 >= 300 {print $2, $3, $4}' "$samples" | tail -n 5)
  (( ${#final[@]} == 5 )) || { echo "soak did not produce five post-five-minute samples" >&2; exit 1; }
  median_column() {
    local column=$1
    printf '%s\n' "${final[@]}" | awk -v column="$column" '{print $column}' | sort -n | sed -n '3p'
  }
  final_rss=$(median_column 1)
  final_fds=$(median_column 2)
  final_threads=$(median_column 3)
  baseline_fds=$(awk -F '\t' 'NR==2 {print $3}' "$samples")
  baseline_threads=$(awk -F '\t' 'NR==2 {print $4}' "$samples")
  rss_limit=$((baseline + 65536))
  percent_limit=$((baseline * 120 / 100))
  (( rss_limit > percent_limit )) || rss_limit=$percent_limit
  (( final_rss <= rss_limit )) || { echo "RSS grew beyond max(64 MiB, 20%): $baseline -> $final_rss KiB" >&2; exit 1; }
  (( final_fds <= baseline_fds + 16 )) || { echo "file descriptors grew by more than 16" >&2; exit 1; }
  (( final_threads <= baseline_threads + 4 )) || { echo "threads grew by more than 4" >&2; exit 1; }
  [[ -z $(find "$spool" -type f -print -quit) ]] || { echo "leftover media spool files detected" >&2; exit 1; }
fi

echo "$profile qualification passed; evidence: $artifact_dir"
