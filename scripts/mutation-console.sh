#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

note() {
  echo "mutation-console: $1" >&2
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then printf '%s\nmutation-console: %s\n%s\n' '```' "$1" '```' >>"$GITHUB_STEP_SUMMARY"; fi
}

if (( $# > 0 )); then
  files=("$@")
else
  base_ref=${MUTATION_BASE:-${RATCHET_BASE:-}}
  if [[ -z $base_ref ]]; then
    for candidate in origin/main main; do
      if git rev-parse --verify --quiet "$candidate^{commit}" >/dev/null; then base_ref=$candidate; break; fi
    done
  fi
  [[ -n $base_ref ]] || { echo "No base ref: set MUTATION_BASE or fetch origin/main" >&2; exit 2; }
  base=$(git merge-base "$base_ref" HEAD)
  mapfile -t candidates < <(
    { git diff --name-only "$base" -- 'console/src/lib/*.ts' 'console/src/lib/**/*.ts'
      git ls-files -o --exclude-standard -- 'console/src/lib/*.ts' 'console/src/lib/**/*.ts'
    } | grep -vE '\.(test|svelte\.test|d)\.ts$' | grep -v '^console/src/lib/api/' | LC_ALL=C sort -u || true
  )
  files=()
  for file in "${candidates[@]}"; do
    if [[ -f $file ]]; then files+=("$file"); fi
  done
fi

if (( ${#files[@]} == 0 )); then
  note "no console module changed, so nothing was mutated"
  exit 0
fi

relative=()
for file in "${files[@]}"; do relative+=("${file#console/}"); done
if (( ${#relative[@]} == 1 )); then
  pattern=${relative[0]}
else
  pattern="{$(IFS=,; echo "${relative[*]}")}"
fi

echo "mutation-console: ${relative[*]}" >&2
pnpm --dir console test:mutation --mutate "$pattern"
