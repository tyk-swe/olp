#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

note() {
  echo "coverage-gate: $1" >&2
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then printf '%s\ncoverage-gate: %s\n%s\n' '```' "$1" '```' >>"$GITHUB_STEP_SUMMARY"; fi
}

base_ref=${1:-${COVERAGE_BASE:-}}
if [[ -z $base_ref ]]; then
  for candidate in origin/main main; do
    if git rev-parse --verify --quiet "$candidate^{commit}" >/dev/null; then base_ref=$candidate; break; fi
  done
fi
[[ -n $base_ref ]] || { echo "No base ref: pass one, set COVERAGE_BASE, or fetch origin/main" >&2; exit 2; }
base=$(git merge-base "$base_ref" HEAD)

mapfile -t dirs < <(
  { git diff --name-only "$base" -- '*.go'
    git ls-files -o --exclude-standard -- '*.go'
  } | grep -E '^(internal|sdk|cmd|plugins)/' | while IFS= read -r file; do dirname -- "$file"; done | LC_ALL=C sort -u
)
packages=()
for dir in "${dirs[@]}"; do
  if [[ -d $dir ]] && compgen -G "$dir/*.go" >/dev/null; then packages+=("./$dir"); fi
done

if (( ${#packages[@]} == 0 )); then
  note "no Go unit package changed, so nothing was compared"
  exit 0
fi

out=.local/coverage-gate
git worktree remove --force "$out/base" >/dev/null 2>&1 || true
git worktree prune
rm -rf -- "$out"
mkdir -p -- "$out"
cleanup() {
  status=$?
  trap - EXIT INT TERM
  git worktree remove --force "$out/base" >/dev/null 2>&1 || true
  git worktree prune
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
git worktree add --detach --quiet "$out/base" "$base"

existing=()
for package in "${packages[@]}"; do
  if compgen -G "$out/base/${package#./}/*.go" >/dev/null; then existing+=("$package"); fi
done
if (( ${#existing[@]} == 0 )); then
  note "changed Go packages are new at head and have no base coverage to drop from"
  exit 0
fi

echo "coverage-gate: comparing ${existing[*]} against ${base:0:12} ($base_ref)" >&2
(cd -- "$out/base" && go test -mod=readonly -timeout=5m -cover "${existing[@]}") >"$out/base.txt" 2>&1 || {
  cat "$out/base.txt" >&2
  echo "coverage-gate: base tests failed" >&2
  exit 1
}
go test -mod=readonly -timeout=5m -cover "${existing[@]}" >"$out/head.txt" 2>&1 || {
  cat "$out/head.txt" >&2
  echo "coverage-gate: head tests failed" >&2
  exit 1
}
node scripts/check-coverage.mjs "$out/base.txt" "$out/head.txt"
