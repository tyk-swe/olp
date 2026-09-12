#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
docker compose -f deploy/compose.dev.yaml up -d --wait
source scripts/local-env.sh
make setup
cargo build --locked --bin olp
./target/debug/olp migrate

pids=()
cleanup() {
  trap - EXIT INT TERM
  if ((${#pids[@]})); then
    kill "${pids[@]}" 2>/dev/null || true
    wait "${pids[@]}" 2>/dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

./target/debug/olp all &
pids+=("$!")
pnpm --dir console dev --host localhost --port 5173 --strictPort &
pids+=("$!")
printf 'Development console: %s\nBootstrap token file: %s\n' "$OLP_PUBLIC_ORIGIN" "$OLP_BOOTSTRAP_TOKEN_FILE"
wait -n "${pids[@]}"
