#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lib/test.sh
unset OLP_QUALIFY_REPORT
start_services
build_binary
export OLP_QUALIFY_LIVE_REPORT="$test_scratch/qualification-live.jsonl"
began=$SECONDS
status=0
go test -mod=readonly -tags 'qualification qualificationlive' -run '^(TestLive|TestCodeModeLive)' -count=1 -timeout=900s -v ./tests/qualification || status=$?
printf 'qualification live lanes: %ss\n' "$((SECONDS - began))"
report="$OLP_QUALIFY_LIVE_REPORT"
if [[ ! -f $report ]]; then
  printf 'live qualification FAILED: no live report was written\n' >&2
  exit 1
fi
cat "$report"
if jq -se 'any(.[]; .evidence == "fixture")' "$report" >/dev/null; then
  printf 'live qualification FAILED: fixture evidence present in the live report\n' >&2
  exit 1
fi
lanes=$(jq -s '[.[] | select(.driver == "protocol-live")] | length' "$report")
if [[ $lanes != 10 ]]; then
  printf 'live qualification FAILED: expected 10 live lane records, found %s\n' "$lanes" >&2
  exit 1
fi
claims=$(jq -c 'select(.type == "claims")' "$report")
if [[ -z $claims ]]; then
  printf 'live qualification FAILED: no claims summary in the live report\n' >&2
  exit 1
fi
printf 'claims summary:\n%s\n' "$claims"
notgreen=$(jq -c 'select(.driver == "protocol-live" and .result != "pass")' "$report")
if [[ -n $notgreen ]]; then
  printf 'live lanes not green (reliability claim fails):\n%s\n' "$notgreen" >&2
  exit 1
fi
exit "$status"
