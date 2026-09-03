#!/usr/bin/env bash
set -euo pipefail

: "${BENCH_RESULT:?BENCH_RESULT is required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"

if jq -e '.valid == true' "$BENCH_RESULT" > /dev/null; then
  validity=valid
else
  validity=invalid
fi

echo "artifact_name=olp-perf-${validity}-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}" >> "$GITHUB_OUTPUT"
