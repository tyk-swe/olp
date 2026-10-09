#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/../../.." && pwd)
fixture_secret_dir=${OLP_CONSOLE_E2E_FLEET_INSTANCE_SECRET_DIR:?fleet instance secret directory required}
unset OLP_AUTH_HMAC_KEY_FILE OLP_BOOTSTRAP_TOKEN_FILE OLP_MASTER_KEY_FILE
# shellcheck source=scripts/secrets.sh
source "$repo_dir/scripts/secrets.sh" "$fixture_secret_dir"
fixture_pid=
cleanup() {
  if [[ -n $fixture_pid ]]; then
    kill -TERM "$fixture_pid" 2>/dev/null || true
    wait "$fixture_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT
trap 'exit 0' INT TERM
"$script_dir/../journeys/run-olp.sh" &
fixture_pid=$!
wait "$fixture_pid"
