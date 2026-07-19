#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/../.." && pwd)

if [[ ! -d "$script_dir/node_modules" ]]; then
  echo "SDK dependencies are missing; run 'pnpm install --frozen-lockfile' in $script_dir" >&2
  exit 1
fi

metadata=$(mktemp)
fixture_log=$(mktemp)
fixture_pid=
cleanup() {
  if [[ -n "$fixture_pid" ]] && kill -0 "$fixture_pid" 2>/dev/null; then
    kill "$fixture_pid" 2>/dev/null || true
    wait "$fixture_pid" 2>/dev/null || true
  fi
  rm -f -- "$metadata" "$fixture_log"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

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
  OLP_SDK_SMOKE_METADATA="$metadata" \
    "$fixture_bin"
) >"$fixture_log" 2>&1 &
fixture_pid=$!

ready=false
for _ in $(seq 1 600); do
  if ! kill -0 "$fixture_pid" 2>/dev/null; then
    echo "SDK smoke fixture exited before becoming ready" >&2
    sed -n '1,240p' "$fixture_log" >&2
    exit 1
  fi
  if [[ -s "$metadata" ]] && node -e 'JSON.parse(require("node:fs").readFileSync(process.argv[1], "utf8"))' "$metadata" 2>/dev/null; then
    ready=true
    break
  fi
  sleep 0.1
done

if [[ "$ready" != true ]]; then
  echo "SDK smoke fixture did not become ready within 60 seconds" >&2
  sed -n '1,240p' "$fixture_log" >&2
  exit 1
fi

OLP_SDK_SMOKE_METADATA="$metadata" node "$script_dir/smoke.mjs"
