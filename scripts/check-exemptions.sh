#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

base=$(git merge-base HEAD "${EXEMPTIONS_BASE:-origin/main}")
list=console/eslint-legacy.json
before=$(git show "$base:$list" 2>/dev/null || echo '{}')
grown=$(jq -r --argjson before "$before" '
  to_entries[]
  | select($before[.key] != null and (.value | length) > ($before[.key] | length))
  | "\(.key) grew from \($before[.key] | length) to \(.value | length)"' "$list")
if [[ -n $grown ]]; then
  printf 'check-exemptions: %s %s\n' "$list" "$grown" >&2
  echo "check-exemptions: legacy lists only shrink. Fix the new violation instead of listing the file." >&2
  exit 1
fi
