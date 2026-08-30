#!/usr/bin/env bash
# Structural gate for docs/compatibility.md: the generated block is present
# exactly once and well formed, and the handwritten notes cite repository
# paths that still exist. Content drift between the table and the endpoint
# registry is the nextest suite's job
# (apps/olp/tests/integration/compatibility_drift.rs); this stays a pure
# shell check so check-static needs no toolchain.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$script_dir/.." && pwd)
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"
VALIDATION_SCRIPT_NAME=check-compatibility-doc.sh
cd "$root"

for required_executable in awk grep sort tr mktemp; do
  validation_require_executable "$required_executable"
done

document=docs/compatibility.md
start_marker='<!-- generated:compatibility:start -->'
end_marker='<!-- generated:compatibility:end -->'
validation_require_file "$document"
validation_require_file README.md

violations=0
fail() {
  printf '%s: %s\n' "$(validation_script_name)" "$1" >&2
  violations=1
}

# grep -c exits 1 on a clean no-match, which is a count of zero here rather
# than a scan failure; anything above 1 aborts instead of reading as "absent".
checked_grep() {
  local output status
  output=$(grep "$@") && status=0 || status=$?
  case $status in
    0 | 1) printf '%s' "$output" ;;
    *)
      printf '%s: scan failed: exit=%d args=%s\n' \
        "$(validation_script_name)" "$status" "$*" >&2
      exit "$status"
      ;;
  esac
}

start_count=$(checked_grep -c -F -x -- "$start_marker" "$document")
end_count=$(checked_grep -c -F -x -- "$end_marker" "$document")
if ((start_count != 1 || end_count != 1)); then
  fail "$document must contain each generated marker exactly once: start=$start_count end=$end_count"
  exit 1
fi

start_line=$(checked_grep -n -F -x -- "$start_marker" "$document" | cut -d: -f1)
end_line=$(checked_grep -n -F -x -- "$end_marker" "$document" | cut -d: -f1)
((start_line < end_line)) \
  || fail "the generated markers are inverted: start=$start_line end=$end_line"

generated=
handwritten=
cleanup() { rm -f "$generated" "$handwritten"; }
trap cleanup EXIT
generated=$(mktemp)
handwritten=$(mktemp)
awk -v start="$start_marker" -v end="$end_marker" \
  -v generated="$generated" -v handwritten="$handwritten" '
  $0 == start { inside = 1; next }
  $0 == end { inside = 0; next }
  inside { print > generated; next }
  { print > handwritten }
' "$document"

((start_line + 1 < end_line)) || fail "the generated section of $document is empty"
for heading in '### OpenAI surface' '### Anthropic surface' '### Gemini surface'; do
  [[ -n $(checked_grep -F -x -- "$heading" "$generated") ]] \
    || fail "the generated section is missing the heading: $heading"
done
[[ -n $(checked_grep -F -- '| native |' "$generated") ]] \
  || fail "the generated section has no native cell; the export is degenerate"

# Every repository path the handwritten notes cite must still exist, so a
# renamed fixture or connector cannot leave the prose pointing at nothing.
backtick=$(printf '\140')
backticked=$(checked_grep -oE "${backtick}[^${backtick}]+${backtick}" "$handwritten" \
  | tr -d "$backtick" | sort -u)
cited=$(checked_grep -E '^(apps|crates|docs|scripts|tests)/[A-Za-z0-9._/-]*$' <<< "$backticked")
[[ -n $cited ]] || fail "the handwritten notes cite no repository paths"
while IFS= read -r path; do
  if [[ -n $path && ! -e $path ]]; then
    fail "the handwritten notes cite a path that does not exist: $path"
  fi
done <<< "$cited"

[[ -n $(checked_grep -F -- 'docs/compatibility.md' README.md) ]] \
  || fail "README.md does not link $document"

((violations == 0)) || exit 1
echo "the compatibility document structure is intact"
