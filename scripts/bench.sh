#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

for command in cargo git oha psql python3; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required benchmark command is unavailable: $command" >&2
    exit 1
  }
done

if command -v valkey-cli >/dev/null 2>&1; then
  OLP_BENCH_VALKEY_CLI=valkey-cli
elif command -v redis-cli >/dev/null 2>&1; then
  OLP_BENCH_VALKEY_CLI=redis-cli
else
  echo "required benchmark command is unavailable: valkey-cli or redis-cli" >&2
  exit 1
fi
export OLP_BENCH_VALKEY_CLI

oha_version="$(oha --version | awk 'NR == 1 { print $2 }')"
if [[ $oha_version != 1.12.0 ]]; then
  echo "oha 1.12.0 is required, found $oha_version" >&2
  exit 1
fi

if [[ -z ${OLP_BENCH_BIN:-} ]]; then
  (cd -- "$repo_root" && SQLX_OFFLINE=true cargo build --locked --release -p olp --features test-util)
  # shellcheck source=scripts/lib/cargo-target-dir.sh
  # shellcheck disable=SC1091
  source "$repo_root/scripts/lib/cargo-target-dir.sh"
  OLP_BENCH_BIN="$(cargo_target_dir "$repo_root")/release/olp"
  OLP_BENCH_BUILD_PROFILE=release
else
  OLP_BENCH_BUILD_PROFILE=${OLP_BENCH_BUILD_PROFILE:-external}
fi

export OLP_BENCH_BIN OLP_BENCH_BUILD_PROFILE
exec python3 "$repo_root/scripts/bench.py" "$@"
