#!/usr/bin/env bash
# Refuses a node suite that has not qualified anything, from the list of tests
# that lib/leaf-tests.mjs reported for it: a test file that defines nothing,
# which is all a suite that ran nothing is made of, and any test or group that
# was skipped or left to do. An open item belongs in the table of run.sh with
# its reason, never in a test.
#
#   lib/check-ran.sh suite tests.jsonl test-file...
set -euo pipefail

if (( $# < 3 )); then
  echo "usage: check-ran.sh suite tests.jsonl test-file..." >&2
  exit 64
fi
name=$1 list=$2
shift 2

# A list that cannot be read lists nothing, so every file defines none.
for file in "$@"; do
  jq -se --arg file "${file##*/}" 'any(.[]; .file == $file)' "$list" >/dev/null 2>&1 || {
    echo "suite $name: ${file##*/} defines no tests" >&2
    exit 1
  }
done
skipped=$(jq -s '[.[] | select(.skip or .todo)] | length' "$list")
if (( skipped > 0 )); then
  echo "suite $name skipped $skipped test(s); record the open item in the table of run.sh instead" >&2
  jq -r 'select(.skip or .todo) | "  \(.file): \(.name)"' "$list" >&2
  exit 1
fi
