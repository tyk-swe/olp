#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
scratch=$(mktemp -d)
project="olp-qualify-$$"
compose=(docker compose -p "$project" -f deploy/compose.dev.yaml)
export OLP_POSTGRES_PORT=0 OLP_VALKEY_PORT=0 OLP_CACHE_VALKEY_PORT=0
cleanup() {
  status=$?
  trap - EXIT INT TERM
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
CGO_ENABLED=1 go build -mod=readonly -trimpath -o "$scratch/olp" ./cmd/olp
"${compose[@]}" up -d --wait --wait-timeout 90 postgres valkey
postgres=$("${compose[@]}" port postgres 5432)
valkey=$("${compose[@]}" port valkey 6379)
export OLP_TEST_BINARY="$scratch/olp"
export OLP_TEST_DATABASE_URL="postgres://olp:olp-local@$postgres/olp?sslmode=disable"
export OLP_TEST_VALKEY_URL="redis://:olp-local@$valkey/0"
export OLP_QUALIFY_REPORT="$scratch/qualification.jsonl"
start=$(date +%s)
status=0
go test -mod=readonly -tags qualification -count=1 -timeout=300s -v ./tests/qualification || status=$?
elapsed=$(( $(date +%s) - start ))
printf 'qualification lanes: %ss\n' "$elapsed"
if [[ -f "$OLP_QUALIFY_REPORT" ]]; then
  cat "$OLP_QUALIFY_REPORT"
fi
exit "$status"
