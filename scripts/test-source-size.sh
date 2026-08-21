#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checker="$script_dir/check-source-size.sh"
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

for required_executable in grep mkdir mktemp truncate; do
  command -v "$required_executable" >/dev/null 2>&1 || {
    echo "test-source-size.sh: $required_executable is required" >&2
    exit 1
  }
done

fixture="$test_root/repository"
mkdir -p \
  "$fixture/apps/example/src" \
  "$fixture/crates/example/src" \
  "$fixture/console/src/lib/api"
truncate -s 30000 "$fixture/apps/example/src/at-limit.rs"
printf 'pub fn example() {}\n' > "$fixture/crates/example/src/lib.rs"
printf '<main>example</main>\n' > "$fixture/console/src/App.svelte"
truncate -s 30001 "$fixture/console/src/lib/api/schema.d.ts"

passing_output=$($checker "$fixture")
grep -Fq 'source-size policy is clean (3 files, limit 30000 bytes)' <<<"$passing_output"

truncate -s 30001 "$fixture/console/src/untracked-oversized.ts"
failure_output="$test_root/oversized.log"
if "$checker" "$fixture" >"$failure_output" 2>&1; then
  echo "source-size checker accepted an oversized worktree source" >&2
  exit 1
fi
grep -Fq 'console/src/untracked-oversized.ts (30001 bytes)' "$failure_output"
if grep -Fq 'schema.d.ts' "$failure_output"; then
  echo "source-size checker did not exclude the generated console schema" >&2
  exit 1
fi

missing_root="$test_root/missing-console"
mkdir -p "$missing_root/apps/example/src" "$missing_root/crates/example/src"
if "$checker" "$missing_root" >/dev/null 2>&1; then
  echo "source-size checker accepted a missing production source root" >&2
  exit 1
fi

echo "source-size policy contract tests passed"
