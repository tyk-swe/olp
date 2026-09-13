#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
docker compose -f deploy/compose.go.yaml up -d --wait
make go-setup
mkdir -p .local/bin
CGO_ENABLED=1 go build -o .local/bin/olp ./cmd/olp
export OLP_DATABASE_URL="postgres://olp_go:olp-go-local@127.0.0.1:${OLP_GO_POSTGRES_PORT:-54321}/olp_go?sslmode=disable"
export OLP_VALKEY_URL="redis://:olp-go-local@127.0.0.1:${OLP_GO_VALKEY_PORT:-63791}/0"
export OLP_LISTEN_ADDR=127.0.0.1:8082
export OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:9092
export OLP_PUBLIC_ORIGIN=http://127.0.0.1:5173
export OLP_DEV_API_ORIGIN=http://127.0.0.1:8082
source scripts/go-secrets.sh "$PWD/.local/go-secrets"
.local/bin/olp migrate
echo "Bootstrap token file: $OLP_BOOTSTRAP_TOKEN_FILE"
pids=()
cleanup() {
  trap - EXIT INT TERM
  if (( ${#pids[@]} )); then
    kill "${pids[@]}" 2>/dev/null || true
    wait "${pids[@]}" 2>/dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
.local/bin/olp all &
pids+=("$!")
pnpm --dir console dev --host 127.0.0.1 &
pids+=("$!")
wait -n "${pids[@]}"
