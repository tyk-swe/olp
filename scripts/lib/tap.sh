#!/usr/bin/env bash

# Shared TAP output for the scripts/ self-tests.
#
# run_test numbers each case in source order and prints the `ok` or `not ok`
# line for it; a failing case keeps its exit status so `set -e` stops the run
# at the first failure. tap_plan closes the stream with the count that ran.

tests_run=0

run_test() {
  local name=$1
  shift

  tests_run=$((tests_run + 1))
  if "$@"; then
    printf 'ok %d - %s\n' "$tests_run" "$name"
  else
    local status=$?
    printf 'not ok %d - %s (exit %d)\n' "$tests_run" "$name" "$status" >&2
    return "$status"
  fi
}

tap_plan() {
  printf '1..%d\n' "$tests_run"
}
