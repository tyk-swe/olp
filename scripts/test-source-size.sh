#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

fixture="$test_root/repository"
mkdir -p \
  "$fixture/apps/example/src" \
  "$fixture/crates/example/src" \
  "$fixture/console/src/lib/api"
truncate -s 30000 "$fixture/apps/example/src/at-limit.rs"
printf 'pub fn example() {}\n' > "$fixture/crates/example/src/lib.rs"
printf '<main>example</main>\n' > "$fixture/console/src/App.svelte"
truncate -s 30001 "$fixture/console/src/lib/api/schema.d.ts"

output=$("$script_dir/check-source-size.sh" "$fixture")
grep -Fq 'source-size policy is clean (3 files, limit 30000 bytes)' <<<"$output"

truncate -s 30001 "$fixture/console/src/oversized.ts"
if "$script_dir/check-source-size.sh" "$fixture" >"$test_root/failure.log" 2>&1; then
  echo "source-size checker accepted an oversized file" >&2
  exit 1
fi
grep -Fq 'console/src/oversized.ts (30001 bytes)' "$test_root/failure.log"
grep -Fq 'schema.d.ts' "$test_root/failure.log" && exit 1

mkdir -p "$test_root/missing-console/apps/example/src" "$test_root/missing-console/crates/example/src"
if "$script_dir/check-source-size.sh" "$test_root/missing-console" >/dev/null 2>&1; then
  echo "source-size checker accepted missing production roots" >&2
  exit 1
fi

echo "source-size policy contract tests passed"
