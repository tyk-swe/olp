# shellcheck shell=bash
# Shared resolution of the cargo target directory for harness scripts.
# Source this file, then call:
#
#   cargo_target_dir REPO_ROOT
#
# Prints the absolute target directory, honoring CARGO_TARGET_DIR whether
# it is absolute or repo-relative.
cargo_target_dir() {
  local repo_root=$1
  local target_dir=${CARGO_TARGET_DIR:-target}
  if [[ $target_dir != /* ]]; then
    target_dir="$repo_root/$target_dir"
  fi
  printf '%s\n' "$target_dir"
}
