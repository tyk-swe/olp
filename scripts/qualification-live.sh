#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
scratch=$(mktemp -d)
project="olp-qualify-live-$$"
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
unset OLP_QUALIFY_REPORT
CGO_ENABLED=1 go build -mod=readonly -trimpath -o "$scratch/olp" ./cmd/olp
"${compose[@]}" up -d --wait --wait-timeout 90 postgres valkey
postgres=$("${compose[@]}" port postgres 5432)
valkey=$("${compose[@]}" port valkey 6379)
export OLP_TEST_BINARY="$scratch/olp"
export OLP_TEST_DATABASE_URL="postgres://olp:olp-local@$postgres/olp?sslmode=disable"
export OLP_TEST_VALKEY_URL="redis://:olp-local@$valkey/0"
export OLP_QUALIFY_LIVE_REPORT="$scratch/qualification-live.jsonl"
start=$(date +%s)
status=0
go test -mod=readonly -tags 'qualification qualificationlive' -run '^(TestLive|TestCodeModeLive)' -count=1 -timeout=900s -v ./tests/qualification || status=$?
elapsed=$(( $(date +%s) - start ))
printf 'qualification live lanes: %ss\n' "$elapsed"
report="$OLP_QUALIFY_LIVE_REPORT"
if [[ ! -f "$report" ]]; then
  printf 'live qualification FAILED: no live report was written\n' >&2
  exit 1
fi
cat "$report"
if grep -q '"evidence":"fixture"' "$report"; then
  printf 'live qualification FAILED: fixture evidence present in the live report\n' >&2
  exit 1
fi
lanes=$(grep -c '"driver":"protocol-live"' "$report" || true)
if [[ "$lanes" != "10" ]]; then
  printf 'live qualification FAILED: expected 10 live lane records, found %s\n' "$lanes" >&2
  exit 1
fi
if ! grep -q '"type":"claims"' "$report"; then
  printf 'live qualification FAILED: no claims summary in the live report\n' >&2
  exit 1
fi
printf 'claims summary:\n'
grep '"type":"claims"' "$report"
notgreen=$(grep '"driver":"protocol-live"' "$report" | grep -v '"result":"pass"' || true)
if [[ -n "$notgreen" ]]; then
  printf 'live lanes not green (reliability claim fails):\n%s\n' "$notgreen" >&2
  exit 1
fi
exit "$status"
