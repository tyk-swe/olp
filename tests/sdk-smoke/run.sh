#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/../.." && pwd)
ready_timeout_seconds=${OLP_SDK_SMOKE_READY_TIMEOUT_SECONDS:-60}
smoke_timeout_seconds=${OLP_SDK_SMOKE_TIMEOUT_SECONDS:-120}

for setting in \
  "OLP_SDK_SMOKE_READY_TIMEOUT_SECONDS:$ready_timeout_seconds" \
  "OLP_SDK_SMOKE_TIMEOUT_SECONDS:$smoke_timeout_seconds"; do
  name=${setting%%:*}
  value=${setting#*:}
  if [[ ! $value =~ ^[1-9][0-9]*$ ]]; then
    echo "$name must be a positive integer" >&2
    exit 64
  fi
done

for command in cargo node timeout; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

if [[ ! -d "$script_dir/node_modules" ]]; then
  echo "SDK dependencies are missing; run 'pnpm install --frozen-lockfile' in $script_dir" >&2
  exit 1
fi

metadata=$(mktemp)
fixture_log=$(mktemp)
fixture_pid=

stop_fixture() {
  if [[ -z $fixture_pid ]]; then
    return 0
  fi

  if kill -0 "$fixture_pid" 2>/dev/null; then
    kill "$fixture_pid" 2>/dev/null || true
    for _ in {1..50}; do
      kill -0 "$fixture_pid" 2>/dev/null || break
      sleep 0.1
    done
    if kill -0 "$fixture_pid" 2>/dev/null; then
      kill -KILL "$fixture_pid" 2>/dev/null || true
    fi
  fi
  wait "$fixture_pid" 2>/dev/null || true
  fixture_pid=
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  stop_fixture
  if (( status != 0 )) && [[ -s $fixture_log ]]; then
    echo "--- SDK smoke fixture log ---" >&2
    tail -n 240 "$fixture_log" >&2
  fi
  rm -f -- "$metadata" "$fixture_log"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

(
  cd -- "$repo_dir"
  cargo build --locked -p olp --example sdk_smoke_fixture
)

target_dir=${CARGO_TARGET_DIR:-target}
if [[ $target_dir != /* ]]; then
  target_dir="$repo_dir/$target_dir"
fi
fixture_bin="$target_dir/debug/examples/sdk_smoke_fixture"
[[ -x $fixture_bin ]] || {
  echo "SDK smoke fixture binary is missing after compilation: $fixture_bin" >&2
  exit 1
}

(
  cd -- "$repo_dir"
  OLP_SDK_SMOKE_METADATA="$metadata" "$fixture_bin"
) >"$fixture_log" 2>&1 &
fixture_pid=$!

ready=false
deadline=$((SECONDS + ready_timeout_seconds))
while (( SECONDS < deadline )); do
  if ! kill -0 "$fixture_pid" 2>/dev/null; then
    echo "SDK smoke fixture exited before becoming ready" >&2
    exit 1
  fi
  if [[ -s $metadata ]] \
    && node -e 'JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"))' \
      "$metadata" 2>/dev/null; then
    ready=true
    break
  fi
  sleep 0.1
done

if [[ $ready != true ]]; then
  echo "SDK smoke fixture did not become ready within ${ready_timeout_seconds} seconds" >&2
  exit 1
fi

if OLP_SDK_SMOKE_METADATA="$metadata" \
  timeout --kill-after=15s "${smoke_timeout_seconds}s" \
    node "$script_dir/smoke.mjs"; then
  :
else
  status=$?
  if (( status == 124 )); then
    echo "SDK smoke client timed out after ${smoke_timeout_seconds} seconds" >&2
  else
    echo "SDK smoke client failed with status $status" >&2
  fi
  exit "$status"
fi
