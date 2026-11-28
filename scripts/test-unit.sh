#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lib/test.sh

console_tests() {
  local script_tests=()
  shopt -s nullglob
  script_tests=(scripts/*.test.mjs)
  shopt -u nullglob
  pnpm --dir console test --project unit --project svelte "$@" &&
    node --test "${script_tests[@]}"
}

case ${1:-} in
  go)
    shift
    if [[ $# -eq 0 ]]; then
      go_suite 'unit go' 30 -vet=off -timeout=60s ./...
    else
      go_suite 'unit go' 30 -vet=off -timeout=60s "$@"
    fi
    ;;
  console)
    shift
    pnpm --dir console api:generate
    timed_suite 'unit console' 30 console_tests "$@"
    ;;
  *)
    printf 'usage: %s go|console [runner arguments]\n' "$0" >&2
    exit 2
    ;;
esac
