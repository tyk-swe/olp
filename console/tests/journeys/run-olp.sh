#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
console_dir=$(cd -- "$script_dir/../.." && pwd)
repo_dir=$(cd -- "$console_dir/.." && pwd)

if [[ -n ${OLP_CONSOLE_E2E_IMAGE:-} ]]; then
  # shellcheck source=scripts/lib/image-container.sh
  source "$repo_dir/scripts/lib/image-container.sh"
  olp_image_container_args
  container_args=(--rm "${olp_image_args[@]}"
    --tmpfs /var/lib/olp/media:rw,nosuid,nodev,size=1280m,mode=0700,uid="$(id -u)",gid="$(id -g)")
  while IFS= read -r name; do
    [[ $name == OLP_* && $name != OLP_CONSOLE_DIR ]] && container_args+=(--env "$name")
  done < <(compgen -e)
  container_args+=(--env OLP_MEDIA_SPOOL_DIR=/var/lib/olp/media)
  docker run "${container_args[@]}" "$OLP_CONSOLE_E2E_IMAGE" migrate
  exec docker run "${container_args[@]}" "$OLP_CONSOLE_E2E_IMAGE" all
fi

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
    cargo build --locked -p olp --features test-util --bin olp
  )
fi
[[ -x $olp_bin ]] || {
  echo "Rust-hosted console integration binary is missing: $olp_bin" >&2
  exit 1
}

cd -- "$console_dir"
"$olp_bin" migrate
exec "$olp_bin" all
