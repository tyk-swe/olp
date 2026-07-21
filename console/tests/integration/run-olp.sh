#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
console_dir=$(cd -- "$script_dir/../.." && pwd)
repo_dir=$(cd -- "$console_dir/.." && pwd)

target_dir=${CARGO_TARGET_DIR:-target}
if [[ $target_dir != /* ]]; then
  target_dir="$repo_dir/$target_dir"
fi
olp_bin=${OLP_CONSOLE_E2E_BIN:-$target_dir/debug/olp}
if [[ $olp_bin != /* ]]; then
  olp_bin="$repo_dir/$olp_bin"
fi

if [[ -z ${OLP_CONSOLE_E2E_BIN:-} ]]; then
  (
    cd -- "$repo_dir"
    cargo build --locked -p olp
  )
fi
[[ -x $olp_bin ]] || {
  echo "Rust-hosted console integration binary is missing: $olp_bin" >&2
  exit 1
}

cd -- "$console_dir"
"$olp_bin" migrate
exec "$olp_bin" control
