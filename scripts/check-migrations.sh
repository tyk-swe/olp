#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C
cd "$(dirname "$0")/.."

directory=internal/platform/database/migrations
base=${MIGRATIONS_BASE:-origin/main}

if ! released=$(git ls-tree --name-only "$base:$directory"); then
  printf 'cannot read migrations on %s, fetch it or set MIGRATIONS_BASE\n' "$base" >&2
  exit 1
fi
current=$(ls -A "$directory")
highest=$(cut -c1-4 <<<"$released" | sort | tail -n 1)

problems=$(
  { grep -Ev '^[0-9]{4}_[a-z0-9_]+\.sql$' <<<"$current" || true; } |
    sed 's/$/ is not named NNNN_name.sql/'
  cut -c1-4 <<<"$current" | sort | uniq -d |
    sed 's/^/more than one migration is numbered /'
  comm -23 <(sort <<<"$current") <(sort <<<"$released") |
    awk -v highest="$highest" -v base="$base" \
      'substr($0, 1, 4) <= highest { print $0 " must be numbered above " highest ", the highest on " base }'
)

if [[ -n $problems ]]; then
  printf '%s\n' "$problems" >&2
  exit 1
fi
