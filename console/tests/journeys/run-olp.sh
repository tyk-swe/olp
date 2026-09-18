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

olp_bin=${OLP_CONSOLE_E2E_BIN:-$repo_dir/.local/bin/olp}
[[ $olp_bin == /* ]] || olp_bin="$repo_dir/$olp_bin"
[[ -x $olp_bin ]] || { echo 'Run make build before browser qualification.' >&2; exit 1; }
cd -- "$repo_dir"
"$olp_bin" migrate
exec "$olp_bin" all
