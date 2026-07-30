#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
console_dir=$(cd -- "$script_dir/../.." && pwd)
repo_dir=$(cd -- "$console_dir/.." && pwd)

# shellcheck source=scripts/lib/cargo-target-dir.sh
source "$repo_dir/scripts/lib/cargo-target-dir.sh"
target_dir=$(cargo_target_dir "$repo_dir")
olp_bin=${OLP_CONSOLE_E2E_BIN:-$target_dir/debug/olp}
if [[ $olp_bin != /* ]]; then
  olp_bin="$repo_dir/$olp_bin"
fi

if [[ -z ${OLP_CONSOLE_E2E_BIN:-} ]]; then
  (
    cd -- "$repo_dir"
    cargo build --locked -p olp --features test-util
  )
fi
[[ -x $olp_bin ]] || {
  echo "Rust-hosted console integration binary is missing: $olp_bin" >&2
  exit 1
}

cd -- "$console_dir"
node tests/integration/assert-empty-valkey.mjs
"$olp_bin" migrate
exec "$olp_bin" all
