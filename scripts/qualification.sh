#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lib/test.sh
start_services
build_binary
export OLP_QUALIFY_REPORT="$test_scratch/qualification.jsonl"
start=$(date +%s)
status=0
go test -mod=readonly -tags qualification -count=1 -timeout=300s -v ./tests/qualification || status=$?
elapsed=$(( $(date +%s) - start ))
printf 'qualification lanes: %ss\n' "$elapsed"
if [[ -f "$OLP_QUALIFY_REPORT" ]]; then
  cat "$OLP_QUALIFY_REPORT"
fi
exit "$status"
