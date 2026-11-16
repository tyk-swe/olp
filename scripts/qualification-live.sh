#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lib/test.sh
unset OLP_QUALIFY_REPORT
start_services
build_binary
export OLP_QUALIFY_LIVE_REPORT="$test_scratch/qualification-live.jsonl"
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
