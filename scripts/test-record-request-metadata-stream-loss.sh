#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
recorder="$script_dir/record-request-metadata-stream-loss.sh"

"$recorder" --help 2>&1 \
  | grep -Fq 'OLP_CONFIRM_REQUEST_METADATA_STREAM_LOSS=record-explicit-gap'
grep -Fq "'request_metadata.stream_loss_recorded'" "$recorder"
if [[ -e "$script_dir/checkpoint-lost-usage-stream.sh" ]]; then
  echo "obsolete request metadata recorder is still present" >&2
  exit 1
fi

echo "request metadata loss recorder contract tests passed"
