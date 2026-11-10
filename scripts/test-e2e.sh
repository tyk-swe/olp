#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lib/test.sh

case ${1:-} in
  api)
    shift
    start_services
    build_binary
    go_suite 'e2e api' 45 -tags e2e -parallel 8 -timeout=180s ./tests/e2e "$@"
    ;;
  browser)
    shift
    start_services
    build_binary
    start_console_stack
    timed_suite 'e2e browser' 60 pnpm --dir console exec playwright test "$@"
    ;;
  *)
    printf 'usage: %s api|browser [runner arguments]\n' "$0" >&2
    exit 2
    ;;
esac
