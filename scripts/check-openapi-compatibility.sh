#!/usr/bin/env bash
set -euo pipefail

workspace_root="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$workspace_root"

for command in jq sha256sum; do
  command -v "$command" >/dev/null || {
    echo "OpenAPI compatibility check requires $command" >&2
    exit 2
  }
done

baseline=docs/enterprise/contracts/baselines/management-v1.0.0.json
current=openapi/management.json
comparator=scripts/openapi-compatibility.jq
expected_baseline_sha256=fc83a8c69977270ce20b65a2fad8b3c73deea916b4ffc589b111176fcf6a753d

for required_file in "$baseline" "$current" "$comparator"; do
  [[ -f $required_file ]] || {
    echo "OpenAPI compatibility input is missing: $required_file" >&2
    exit 1
  }
done

actual_baseline_sha256=$(sha256sum "$baseline")
actual_baseline_sha256=${actual_baseline_sha256%% *}
[[ $actual_baseline_sha256 == "$expected_baseline_sha256" ]] || {
  echo "frozen OpenAPI baseline was edited in place: $baseline" >&2
  exit 1
}

comparison=$(jq --null-input \
  --slurpfile baseline "$baseline" \
  --slurpfile current "$current" \
  --from-file "$comparator")

if ! jq --exit-status '.compatible == true' <<<"$comparison" >/dev/null; then
  echo "management OpenAPI is incompatible with frozen v1.0.0:" >&2
  jq -r '.violations[] | "  - \(.)"' <<<"$comparison" >&2
  exit 1
fi

echo "management OpenAPI is backward-compatible with frozen v1.0.0"
