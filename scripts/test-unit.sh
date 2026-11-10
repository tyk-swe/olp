#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lib/test.sh

console_tests() {
  pnpm --dir console test --project unit --project svelte &&
    node --test scripts/check-docs.test.mjs scripts/check-release-version.test.mjs
}

case ${1:-} in
  go) go_suite 'unit go' 30 -vet=off -timeout=60s ./... ;;
  console) timed_suite 'unit console' 30 console_tests ;;
  *)
    printf 'usage: %s go|console\n' "$0" >&2
    exit 2
    ;;
esac
