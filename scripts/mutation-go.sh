#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
root=$PWD

gremlins=github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0
efficacy=${MUTATION_EFFICACY:-70}
mcover=${MUTATION_MCOVER:-40}
workers=${MUTATION_WORKERS:-4}
out=.local/mutation-go
mode=diff
base=

note() {
  echo "mutation-go: $1" >&2
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then printf '%s\nmutation-go: %s\n%s\n' '```' "$1" '```' >>"$GITHUB_STEP_SUMMARY"; fi
}

if (( $# > 0 )) && [[ $1 == --all ]]; then
  mode=packages
  mapfile -t packages < <(go list -f '{{.Dir}}' ./internal/... ./sdk/... | sed "s|^$PWD/||" | LC_ALL=C sort -u)
elif (( $# > 0 )); then
  mode=packages
  packages=("$@")
else
  base_ref=${MUTATION_BASE:-${RATCHET_BASE:-}}
  if [[ -z $base_ref ]]; then
    for candidate in origin/main main; do
      if git rev-parse --verify --quiet "$candidate^{commit}" >/dev/null; then base_ref=$candidate; break; fi
    done
  fi
  [[ -n $base_ref ]] || { echo "No base ref: set MUTATION_BASE or fetch origin/main" >&2; exit 2; }
  base=$(git merge-base "$base_ref" HEAD)
  mapfile -t packages < <(
    { git diff --name-only --diff-filter=d "$base" -- '*.go'
      git ls-files -o --exclude-standard -- '*.go'
    } | grep -E '^(internal|sdk|cmd|plugins)/' | while IFS= read -r file; do dirname -- "$file"; done | LC_ALL=C sort -u
  )
fi

if (( ${#packages[@]} == 0 )); then
  note "no Go unit package changed, so nothing was mutated"
  exit 0
fi

git worktree remove --force "$out/src" >/dev/null 2>&1 || true
git worktree prune
rm -rf -- "$out"
mkdir -p -- "$out/tmp"
cleanup() {
  status=$?
  trap - EXIT INT TERM
  git worktree remove --force "$out/src" >/dev/null 2>&1 || true
  git worktree prune
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
git worktree add --detach --quiet "$out/src" HEAD
if ! git diff HEAD --quiet; then
  git diff HEAD --binary | git -C "$out/src" apply
fi
git ls-files -o --exclude-standard | while IFS= read -r file; do
  mkdir -p -- "$out/src/$(dirname -- "$file")"
  cp -- "$file" "$out/src/$file"
done
git -C "$out/src" add -A

status=0
reports=()
if [[ $mode == diff ]]; then
  report="$root/$out/diff.json"
  echo "mutation-go: changed lines against ${base:0:12}, triggered by ${packages[*]} (efficacy >= $efficacy, mutant coverage >= $mcover)" >&2
  (cd -- "$out/src" && TMPDIR="$root/$out/tmp" go run "$gremlins" unleash . \
    --workers "$workers" \
    --diff "$base" \
    --exclude-files '\.gen\.go$' \
    --exclude-files '^tests/' \
    -o "$report") || status=1
  [[ -f $report ]] && reports+=("$report")
else
  ran=0
  for package in "${packages[@]}"; do
    if [[ ! -d $out/src/$package ]]; then
      note "$package does not exist at head and was not mutated"
      continue
    fi
    ran=$((ran + 1))
    report="$root/$out/${package//\//-}.json"
    echo "mutation-go: $package (efficacy >= $efficacy, mutant coverage >= $mcover)" >&2
    (cd -- "$out/src" && TMPDIR="$root/$out/tmp" go run "$gremlins" unleash "./$package" \
      --workers "$workers" \
      --exclude-files '\.gen\.go$' \
      -o "$report") || status=1
    [[ -f $report ]] && reports+=("$report")
  done
  if (( ${#reports[@]} == 0 && ran > 0 )); then
    note "$ran package(s) ran but produced no mutation report"
    status=1
  fi
fi
if (( ${#reports[@]} > 0 )); then
  node scripts/check-mutation.mjs --efficacy "$efficacy" --mcover "$mcover" "${reports[@]}" || status=1
elif [[ $mode == diff ]]; then
  note "the diff run produced no mutation report"
  status=1
fi
exit "$status"
