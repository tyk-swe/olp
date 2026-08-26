#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checker="$script_dir/check-source-size.sh"
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

# shellcheck source=scripts/lib/tap.sh
source "$script_dir/lib/tap.sh"

long_fn() {
  printf 'fn %s() {\n' "$1"
  for _ in $(seq 1 "$2"); do printf '    let _ = "{ not a brace }";\n'; done
  printf '}\n'
}

fixture=$test_root/repo
mkdir -p "$fixture/apps/demo/src" "$fixture/scripts/lib"
cp "$checker" "$fixture/scripts/"
cp "$script_dir/lib/repository-validation.sh" "$fixture/scripts/lib/"
{
  long_fn short 10
  echo '#[cfg(test)]'
  echo 'mod tests {'
  long_fn ignored_in_tests 150
  echo '}'
} > "$fixture/apps/demo/src/lib.rs"
: > "$fixture/scripts/source-size-baseline.txt"

check() {
  (cd "$fixture" && ./scripts/check-source-size.sh "$@")
}

clean_tree_passes() { check >/dev/null; }
run_test "a short function and a long test-only function pass" clean_tree_passes

long_fn too_long 120 > "$fixture/apps/demo/src/long.rs"
new_violation_fails() { ! check >/dev/null 2>&1; }
run_test "a new function over the limit fails" new_violation_fails

update_records_baseline() {
  check --update >/dev/null && grep -q '^fn:apps/demo/src/long.rs:too_long$' "$fixture/scripts/source-size-baseline.txt"
}
run_test "--update grandfathers the violation" update_records_baseline
run_test "a grandfathered violation passes" clean_tree_passes

head -c 31000 /dev/zero | tr '\0' 'x' > "$fixture/apps/demo/src/big.ts"
run_test "a file over 30 KB fails" new_violation_fails
rm "$fixture/apps/demo/src/big.ts"

rm "$fixture/apps/demo/src/long.rs"
run_test "a fixed entry left in the baseline fails" new_violation_fails

tap_plan
